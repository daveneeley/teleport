/*
Copyright 2022 Gravitational, Inc.

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

package identity

import (
	"strings"

	"github.com/gravitational/trace"
	"gopkg.in/yaml.v3"
)

// ArtifactKind is a type of identity artifact that can be stored and loaded.
type ArtifactKind string

const (
	// KindAlways identifies identity resources that should always be
	// generated.
	KindAlways ArtifactKind = "always"

	// KindSSH identifies resources that should only be generated for SSH use.
	KindSSH ArtifactKind = "ssh"

	// KindTLS identifies resources that should only be stored for TLS use.
	KindTLS ArtifactKind = "tls"

	// KindBotInternal identifies resources that should only be stored in the
	// bot's internal data directory.
	KindBotInternal ArtifactKind = "bot-internal"
)

// allConfigKinds is a list of all ArtifactKinds allowed in config files.
var allConfigKinds []string = []string{string(KindSSH), string(KindTLS)}

func (ac *ArtifactKind) UnmarshalYAML(node *yaml.Node) error {
	var kind string
	if err := node.Decode(&kind); err != nil {
		return err
	}

	// Only TLS and SSH are configurable values.
	switch kind {
	case string(KindTLS):
		*ac = KindTLS
	case string(KindSSH):
		*ac = KindSSH
	default:
		return trace.BadParameter(
			"invalid kind %q, expected one of: %s",
			kind, strings.Join([]string(allConfigKinds), ", "),
		)
	}

	return nil
}

// ContainsKind determines if a particular artifact kind is included in the
// list of kinds.
func ContainsKind(kind ArtifactKind, kinds []ArtifactKind) bool {
	for _, k := range kinds {
		if kind == k {
			return true
		}
	}

	return false
}

// BotKinds returns a list of all artifact kinds used internally by the bot.
// End-user destinations may contain a different set of artifacts.
func BotKinds() []ArtifactKind {
	return []ArtifactKind{KindAlways, KindBotInternal, KindSSH, KindTLS}
}
