/*
Copyright 2021-2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gravitational/teleport"
	api "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/tool/tbot/config"
	"github.com/gravitational/teleport/tool/tbot/identity"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/kr/pretty"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

var log = logrus.WithFields(logrus.Fields{
	trace.Component: teleport.ComponentTBot,
})

const (
	authServerEnvVar = "TELEPORT_AUTH_SERVER"
	tokenEnvVar      = "TELEPORT_BOT_TOKEN"

	nologinPrefix = "-teleport-nologin-"
)

func main() {
	if err := Run(os.Args[1:]); err != nil {
		utils.FatalError(err)
		trace.DebugReport(err)
	}
}

func Run(args []string) error {
	var cf config.CLIConf
	utils.InitLogger(utils.LoggingForDaemon, logrus.InfoLevel)

	app := utils.InitCLIParser("tbot", "tbot: Teleport Credential Bot").Interspersed(false)
	app.Flag("debug", "Verbose logging to stdout").Short('d').BoolVar(&cf.Debug)
	app.Flag("config", "tbot.yaml path").Short('c').StringVar(&cf.ConfigPath)

	startCmd := app.Command("start", "Starts the renewal bot, writing certificates to the data dir at a set interval.")
	startCmd.Flag("auth-server", "Specify the Teleport auth server host").Short('a').Envar(authServerEnvVar).StringVar(&cf.AuthServer)
	startCmd.Flag("token", "A bot join token, if attempting to onboard a new bot; used on first connect.").Envar(tokenEnvVar).StringVar(&cf.Token)
	startCmd.Flag("ca-pin", "A repeatable auth server CA hash to pin; used on first connect.").StringsVar(&cf.CAPins)
	startCmd.Flag("data-dir", "Directory to store internal bot data.").StringVar(&cf.DataDir)
	startCmd.Flag("destination-dir", "Directory to write generated certificates").StringVar(&cf.DestinationDir)
	startCmd.Flag("certificate-ttl", "TTL of generated certificates").Default("60m").DurationVar(&cf.CertificateTTL)
	startCmd.Flag("renew-interval", "Interval at which certificates are renewed; must be less than the certificate TTL.").DurationVar(&cf.RenewInterval)

	initCmd := app.Command("init", "Initialize a certificate destination directory for writes from a separate bot user.")
	initCmd.Flag("destination-dir", "Destination directory to initialize.").StringVar(&cf.DestinationDir)
	initCmd.Flag("init-dir", "If multiple destinations are configured, specify which to initialize").StringVar(&cf.InitDir)
	initCmd.Flag("clean", "If set, remove unexpected files and directories from the destination").BoolVar(&cf.Clean)
	initCmd.Arg("bot-user", "Name of the bot Unix user which should have write access to the destination.").Required().StringVar(&cf.BotUser)

	configCmd := app.Command("config", "Parse and dump a config file").Hidden()

	watchCmd := app.Command("watch", "Watch a destination directory for changes.")

	command, err := app.Parse(args)
	if err != nil {
		return trace.Wrap(err)
	}

	// While in debug mode, send logs to stdout.
	if cf.Debug {
		utils.InitLogger(utils.LoggingForDaemon, logrus.DebugLevel)
	}

	botConfig, err := config.FromCLIConf(&cf)
	if err != nil {
		return trace.Wrap(err)
	}

	switch command {
	case startCmd.FullCommand():
		err = onStart(botConfig)
	case configCmd.FullCommand():
		err = onConfig(botConfig)
	case initCmd.FullCommand():
		err = onInit(botConfig, &cf)
	case watchCmd.FullCommand():
		err = onWatch(botConfig)
	default:
		// This should only happen when there's a missing switch case above.
		err = trace.BadParameter("command %q not configured", command)
	}

	return err
}

func onConfig(botConfig *config.BotConfig) error {
	pretty.Println(botConfig)

	return nil
}

func onWatch(botConfig *config.BotConfig) error {
	return trace.NotImplemented("watch not yet implemented")
}

func onStart(botConfig *config.BotConfig) error {
	// Start by loading the bot's primary destination.
	dest, err := botConfig.Storage.GetDestination()
	if err != nil {
		return trace.WrapWithMessage(err, "could not read bot storage destination from config")
	}

	addr, err := utils.ParseAddr(botConfig.AuthServer)
	if err != nil {
		return trace.WrapWithMessage(err, "invalid auth server address %+v", botConfig.AuthServer)
	}

	var authClient *auth.Client

	// First, attempt to load an identity from storage.
	ident, err := identity.LoadIdentity(dest, identity.BotKinds()...)
	if err == nil {
		identStr, err := describeTLSIdentity(ident)
		if err != nil {
			return trace.Wrap(err)
		}

		log.Infof("Successfully loaded bot identity, %s", identStr)

		// TODO: we should cache the token; if --token is provided but
		// different, assume the user is attempting to start with a new
		// identity. (May want to store a sha256 has to avoid storing the
		// token directly.)
		if botConfig.Onboarding != nil {
			log.Warn("Note: onboarding config ignored as identity was loaded from persistent storage")
		}

		authClient, err = authenticatedUserClientFromIdentity(ident, botConfig.AuthServer)
		if err != nil {
			return trace.Wrap(err)
		}
	} else {
		// If the identity can't be loaded, assume we're starting fresh and
		// need to generate our initial identity from a token

		// TODO: validate that errors from LoadIdentity are sanely typed; we
		// actually only want to ignore NotFound errors

		onboarding := botConfig.Onboarding
		if onboarding == nil {
			return trace.BadParameter("onboarding config required on first start via CLI or YAML")
		}

		// If no token is present, we can't continue.
		if onboarding.Token == "" {
			return trace.Errorf("unable to start: no identity could be loaded and no token present")
		}

		tlsPrivateKey, sshPublicKey, tlsPublicKey, err := generateKeys()
		if err != nil {
			return trace.WrapWithMessage(err, "unable to generate new keypairs")
		}

		params := RegisterParams{
			Token:        onboarding.Token,
			Servers:      []utils.NetAddr{*addr}, // TODO: multiple servers?
			PrivateKey:   tlsPrivateKey,
			PublicTLSKey: tlsPublicKey,
			PublicSSHKey: sshPublicKey,
			CipherSuites: utils.DefaultCipherSuites(),
			CAPins:       onboarding.CAPins,
			CAPath:       onboarding.CAPath,
			Clock:        clockwork.NewRealClock(),
		}

		log.Info("Attempting to generate new identity from token")
		ident, err = newIdentityViaAuth(params)
		if err != nil {
			return trace.Wrap(err)
		}

		log.Debug("Attempting first connection using initial auth client")
		authClient, err = authenticatedUserClientFromIdentity(ident, botConfig.AuthServer)
		if err != nil {
			return trace.Wrap(err)
		}

		// Attempt a request to make sure our client works.
		if _, err := authClient.Ping(context.Background()); err != nil {
			return trace.WrapWithMessage(err, "unable to communicate with auth server")
		}

		identStr, err := describeTLSIdentity(ident)
		if err != nil {
			return trace.Wrap(err)
		}
		log.Infof("Successfully generated new bot identity, %s", identStr)

		log.Debugf("Storing new bot identity to %s", dest)
		if err := identity.SaveIdentity(ident, dest, identity.BotKinds()...); err != nil {
			return trace.WrapWithMessage(err, "unable to save generated identity back to destination")
		}
	}

	// TODO: handle cases where an identity exists on disk but we might _not_
	// want to use it:
	//  - identity has expired
	//  - user provided a new token (can now compare to the stored tokenhash)
	//  - ???

	watcher, err := authClient.NewWatcher(context.Background(), types.Watch{
		Kinds: []types.WatchKind{{
			Kind: types.KindCertAuthority,
		}},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	go watchCARotations(watcher)

	defer watcher.Close()

	return renewLoop(botConfig, authClient, ident)
}

func watchCARotations(watcher types.Watcher) {
	for {
		select {
		case event := <-watcher.Events():
			log.Debugf("CA event: %+v", event)
			// TODO: handle CA rotations
		case <-watcher.Done():
			if err := watcher.Error(); err != nil {
				log.WithError(err).Warnf("error watching for CA rotations")
			}
			return
		}
	}
}

// newIdentityViaAuth contacts the auth server directly to exchange a token for
// a new set of user certificates.
func newIdentityViaAuth(params RegisterParams) (*identity.Identity, error) {
	var client *auth.Client
	var rootCAs []*x509.Certificate
	var err error

	// Build a client to the Auth Server. If a CA pin is specified require the
	// Auth Server is validated. Otherwise attempt to use the CA file on disk
	// but if it's not available connect without validating the Auth Server CA.
	switch {
	case len(params.CAPins) != 0:
		client, rootCAs, err = pinAuthClient(params)
	default:
		// TODO: need to handle an empty list of root CAs in this case. Should
		// we just trust on first connect and save the root CA?
		client, rootCAs, err = insecureAuthClient(params)
	}
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer client.Close()
	if err != nil {
		return nil, trace.WrapWithMessage(err, "Could not create an unauthenticated auth client")
	}

	// Note: GenerateInitialRenewableUserCerts will fetch _only_ the cluster's
	// user CA cert. However, to communicate with the auth server, we'll also
	// need to fetch the cluster's host CA cert, which we should have fetched
	// earlier while initializing the auth client.
	certs, err := client.GenerateInitialRenewableUserCerts(context.Background(), proto.RenewableCertsRequest{
		Token:     params.Token,
		PublicKey: params.PublicSSHKey,
	})
	if err != nil {
		return nil, trace.WrapWithMessage(err, "Could not generate initial user certificates")
	}

	// Append any additional root CAs we received as part of the auth process
	// (i.e. the host CA cert)
	for _, cert := range rootCAs {
		pemBytes, err := tlsca.MarshalCertificatePEM(cert)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		certs.TLSCACerts = append(certs.TLSCACerts, pemBytes)
	}

	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(params.Token)))
	loadParams := identity.LoadIdentityParams{
		PrivateKeyBytes: params.PrivateKey,
		PublicKeyBytes:  params.PublicSSHKey,
		TokenHashBytes:  []byte(tokenHash),
	}

	return identity.ReadIdentityFromStore(&loadParams, certs, identity.KindBotInternal, identity.KindTLS)
}

func renewIdentityViaAuth(
	client *auth.Client,
	currentIdentity *identity.Identity,
	certTTL time.Duration,
) (*identity.Identity, error) {
	// TODO: enforce expiration > renewal period (by what margin?)

	// First, ask the auth server to generate a new set of certs with a new
	// expiration date.
	certs, err := client.GenerateUserCerts(context.Background(), proto.UserCertsRequest{
		PublicKey: currentIdentity.PublicKeyBytes,
		Username:  currentIdentity.XCert.Subject.CommonName,
		Expires:   time.Now().Add(certTTL),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// The root CA included with the returned user certs will only contain the
	// Teleport User CA. We'll also need the host CA for future API calls.
	localCA, err := client.GetClusterCACert()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	caCerts, err := tlsca.ParseCertificatePEMs(localCA.TLSCA)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Append the host CAs from the auth server.
	for _, cert := range caCerts {
		pemBytes, err := tlsca.MarshalCertificatePEM(cert)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		certs.TLSCACerts = append(certs.TLSCACerts, pemBytes)
	}

	newIdentity, err := identity.ReadIdentityFromStore(
		currentIdentity.Params(),
		certs,
		identity.KindBotInternal, identity.KindTLS,
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return newIdentity, nil
}

// fetchDefaultRoles requests the bot's own role from the auth server and
// extracts its full list of allowed roles.
func fetchDefaultRoles(ctx context.Context, client *auth.Client, botRole string) ([]string, error) {
	role, err := client.GetRole(ctx, botRole)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	conditions := role.GetImpersonateConditions(types.Allow)
	return conditions.Roles, nil
}

// describeTLSIdentity writes an informational message about the given identity to
// the log.
func describeTLSIdentity(ident *identity.Identity) (string, error) {
	cert := ident.XCert
	if cert == nil {
		return "", trace.BadParameter("attempted to describe TLS identity without TLS credentials")
	}

	tlsIdent, err := tlsca.FromSubject(cert.Subject, cert.NotAfter)
	if err != nil {
		return "", trace.WrapWithMessage(err, "bot TLS certificate can not be parsed as an identity")
	}

	var principals []string
	for _, principal := range tlsIdent.Principals {
		if !strings.HasPrefix(principal, nologinPrefix) {
			principals = append(principals, principal)
		}
	}

	duration := cert.NotAfter.Sub(cert.NotBefore)
	return fmt.Sprintf(
		"valid: after=%v, before=%v, duration=%s | kind=tls, renewable=%v, disallow-reissue=%v, roles=%v, principals=%v",
		cert.NotBefore.Format(time.RFC3339),
		cert.NotAfter.Format(time.RFC3339),
		duration,
		tlsIdent.Renewable,
		tlsIdent.DisallowReissue,
		tlsIdent.Groups,
		principals,
	), nil
}

// describeIdentity writes an informational message about the given identity to
// the log.
func describeSSHIdentity(ident *identity.Identity) (string, error) {
	cert := ident.Cert
	if cert == nil {
		return "", trace.BadParameter("attempted to describe SSH identity without SSH credentials")
	}

	// TODO: pending #10098 (renewable flag not added to SSH certs)
	//renewable := false
	//if _, ok := cert.Extensions[teleport.CertExtensionRenewable]; ok {
	//	renewable = true
	//}

	disallowReissue := false
	if _, ok := cert.Extensions[teleport.CertExtensionDisallowReissue]; ok {
		disallowReissue = true
	}

	var roles []string
	if rolesStr, ok := cert.Extensions[teleport.CertExtensionTeleportRoles]; ok {
		if actualRoles, err := services.UnmarshalCertRoles(rolesStr); err == nil {
			roles = actualRoles
		}
	}

	var principals []string
	for _, principal := range cert.ValidPrincipals {
		if !strings.HasPrefix(principal, nologinPrefix) {
			principals = append(principals, principal)
		}
	}

	duration := time.Second * time.Duration(cert.ValidBefore-cert.ValidAfter)
	return fmt.Sprintf(
		"valid: after=%v, before=%v, duration=%s | kind=ssh, disallow-reissue=%v, roles=%v, principals=%v",
		time.Unix(int64(cert.ValidAfter), 0).Format(time.RFC3339),
		time.Unix(int64(cert.ValidBefore), 0).Format(time.RFC3339),
		duration,
		disallowReissue,
		roles,
		principals,
	), nil
}

func renewLoop(cfg *config.BotConfig, client *auth.Client, ident *identity.Identity) error {
	// TODO: failures here should probably not just end the renewal loop, there
	// should be some retry / back-off logic.

	// TODO: what should this interval be? should it be user configurable?
	// Also, must be < the validity period.
	// TODO: validate that cert is actually renewable.

	log.Infof("Beginning renewal loop: ttl=%s interval=%s", cfg.CertificateTTL, cfg.RenewInterval)
	if cfg.RenewInterval > cfg.CertificateTTL {
		log.Errorf(
			"Certificate TTL (%s) is shorter than the renewal interval (%s). The next renewal is likely to fail.",
			cfg.CertificateTTL,
			cfg.RenewInterval,
		)
	}

	// Determine where the bot should write its internal data (renewable cert
	// etc)
	botDestination, err := cfg.Storage.GetDestination()
	if err != nil {
		return trace.Wrap(err)
	}

	ticker := time.NewTicker(cfg.RenewInterval)
	defer ticker.Stop()
	for {
		log.Debug("Attempting to renew bot certificates...")
		newIdentity, err := renewIdentityViaAuth(client, ident, cfg.CertificateTTL)
		if err != nil {
			return trace.Wrap(err)
		}

		identStr, err := describeTLSIdentity(ident)
		if err != nil {
			return trace.WrapWithMessage(err, "Could not describe bot identity at %s", botDestination)
		}

		log.Infof("Successfully renewed bot certificates, %s", identStr)

		// TODO: warn if duration < certTTL? would indicate TTL > server's max renewable cert TTL
		// TODO: error if duration < renewalInterval? next renewal attempt will fail

		// Immediately attempt to reconnect using the new identity (still
		// haven't persisted the known-good certs).
		newClient, err := authenticatedUserClientFromIdentity(newIdentity, cfg.AuthServer)
		if err != nil {
			return trace.Wrap(err)
		}

		// Attempt a request to make sure our client works.
		// TODO: consider a retry/backoff loop.
		if _, err := newClient.Ping(context.Background()); err != nil {
			return trace.WrapWithMessage(err, "unable to communicate with auth server")
		}

		log.Debug("Auth client now using renewed credentials.")
		client = newClient
		ident = newIdentity

		// Now that we're sure the new creds work, persist them.
		if err := identity.SaveIdentity(newIdentity, botDestination, identity.BotKinds()...); err != nil {
			return trace.Wrap(err)
		}

		// Determine the default role list based on the bot role. The role's
		// name should match the certificate's Key ID (user and role names
		// should all match bot-$name)
		botResourceName := ident.XCert.Subject.CommonName
		defaultRoles, err := fetchDefaultRoles(context.Background(), client, botResourceName)
		if err != nil {
			log.WithError(err).Warnf("Unable to determine default roles, no roles will be requested if unspecified")
			defaultRoles = []string{}
		}

		// Next, generate impersonated certs
		expires := ident.XCert.NotAfter
		for _, dest := range cfg.Destinations {
			destImpl, err := dest.GetDestination()
			if err != nil {
				return trace.Wrap(err)
			}

			var desiredRoles []string
			if len(dest.Roles) > 0 {
				desiredRoles = dest.Roles
			} else {
				log.Debugf("Destination specified no roles, defaults will be requested: %v", defaultRoles)
				desiredRoles = defaultRoles
			}

			impersonatedIdent, err := generateImpersonatedIdentity(client, ident, expires, desiredRoles, dest.Kinds)
			if err != nil {
				return trace.WrapWithMessage(err, "Failed to generate impersonated certs for %s: %+v", destImpl, err)
			}

			var impersonatedIdentStr string
			if dest.ContainsKind(identity.KindTLS) {
				impersonatedIdentStr, err = describeTLSIdentity(impersonatedIdent)
				if err != nil {
					return trace.WrapWithMessage(err, "could not describe impersonated certs for destination %s", destImpl)
				}
			} else {
				// Note: kinds must contain at least 1 of TLS or SSH
				impersonatedIdentStr, err = describeSSHIdentity(impersonatedIdent)
				if err != nil {
					return trace.WrapWithMessage(err, "could not describe impersonated certs for destination %s", destImpl)
				}
			}
			log.Infof("Successfully renewed impersonated certificates for %s, %s", destImpl, impersonatedIdentStr)

			if err := identity.SaveIdentity(impersonatedIdent, destImpl, dest.Kinds...); err != nil {
				return trace.WrapWithMessage(err, "failed to save impersonated identity to destination %s", destImpl)
			}

			for _, templateConfig := range dest.Configs {
				template, err := templateConfig.GetConfigTemplate()
				if err != nil {
					return trace.Wrap(err)
				}

				if err := template.Render(client, impersonatedIdent, dest); err != nil {
					log.WithError(err).Warnf("Failed to render config template %+v", templateConfig)
				}
			}
		}

		log.Infof("Persisted new certificates to disk. Next renewal in approximately %s", cfg.RenewInterval)
		<-ticker.C
	}
}

func authenticatedUserClientFromIdentity(id *identity.Identity, authServer string) (*auth.Client, error) {
	tlsConfig, err := id.TLSConfig([]uint16{})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	addr, err := utils.ParseAddr(authServer)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return auth.NewClient(api.Config{
		Addrs: utils.NetAddrsToStrings([]utils.NetAddr{*addr}),
		Credentials: []api.Credentials{
			api.LoadTLS(tlsConfig),
		},
	})
}

func generateImpersonatedIdentity(
	client *auth.Client,
	currentIdentity *identity.Identity,
	expires time.Time,
	roleRequests []string,
	kinds []identity.ArtifactKind,
) (*identity.Identity, error) {
	// TODO: enforce expiration > renewal period (by what margin?)

	// Generate a fresh keypair for the impersonated identity. We don't care to
	// reuse keys here: impersonated certs might not be as well-protected so
	// constantly rotating private keys
	privateKey, publicKey, err := native.GenerateKeyPair("")
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// First, ask the auth server to generate a new set of certs with a new
	// expiration date.
	certs, err := client.GenerateUserCerts(context.Background(), proto.UserCertsRequest{
		PublicKey:    publicKey,
		Username:     currentIdentity.XCert.Subject.CommonName,
		Expires:      expires,
		RoleRequests: roleRequests,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// The root CA included with the returned user certs will only contain the
	// Teleport User CA. We'll also need the host CA for future API calls.
	localCA, err := client.GetClusterCACert()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	caCerts, err := tlsca.ParseCertificatePEMs(localCA.TLSCA)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Append the host CAs from the auth server.
	for _, cert := range caCerts {
		pemBytes, err := tlsca.MarshalCertificatePEM(cert)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		certs.TLSCACerts = append(certs.TLSCACerts, pemBytes)
	}

	newIdentity, err := identity.ReadIdentityFromStore(&identity.LoadIdentityParams{
		PrivateKeyBytes: privateKey,
		PublicKeyBytes:  publicKey,
	}, certs, kinds...)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return newIdentity, nil
}

func generateKeys() (private, sshpub, tlspub []byte, err error) {
	privateKey, publicKey, err := native.GenerateKeyPair("")
	if err != nil {
		return nil, nil, nil, err
	}

	sshPrivateKey, err := ssh.ParseRawPrivateKey(privateKey)
	if err != nil {
		return nil, nil, nil, err
	}

	tlsPublicKey, err := tlsca.MarshalPublicKeyFromPrivateKeyPEM(sshPrivateKey)
	if err != nil {
		return nil, nil, nil, err
	}

	return privateKey, publicKey, tlsPublicKey, nil
}
