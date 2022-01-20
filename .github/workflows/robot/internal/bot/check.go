/*
Copyright 2021 Gravitational, Inc.

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

package bot

import (
	"context"
	"log"
	"strings"

	"github.com/gravitational/trace"
)

// Check checks if required reviewers have approved the PR.
//
// Team specific reviews require an approval from both sets of reviews.
// External reviews require approval from admins.
func (b *Bot) Check(ctx context.Context) error {
	reviews, err := b.c.GitHub.ListReviews(ctx,
		b.c.Environment.Organization,
		b.c.Environment.Repository,
		b.c.Environment.Number)
	if err != nil {
		return trace.Wrap(err)
	}

	if b.c.Review.IsInternal(b.c.Environment.Author) {
		// Remove stale "Check" status badges inline for internal reviews.
		err := b.dismiss(ctx,
			b.c.Environment.Organization,
			b.c.Environment.Repository,
			b.c.Environment.UnsafeBranch)
		if err != nil {
			return trace.Wrap(err)
		}

		docs, code, err := b.parseChanges(ctx)
		if err != nil {
			return trace.Wrap(err)
		}
		if err := b.c.Review.CheckInternal(b.c.Environment.Author, reviews, docs, code); err != nil {
			return trace.Wrap(err)
		}

		if err := b.checkTests(ctx); err != nil {
			message := "Passed code review, but missing test coverage."

			log.Printf("Check: %v", message)
			err := b.c.GitHub.CreateComment(ctx,
				b.c.Environment.Organization,
				b.c.Environment.Repository,
				b.c.Environment.Number)
			if err != nil {
				log.Printf("Check: Failed to leave comment: %v.", err)
			}
			return trace.Wrap(err)
		}
		return nil
	}
	if err := b.c.Review.CheckExternal(b.c.Environment.Author, reviews); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func (b *Bot) checkTests(ctx context.Context) error {
	//if b.c.Review.CheckAdmin() {
	//	return nil
	//}
	//if !hasTestCoverage() {

	//}

	return nil
}

func (b *Bot) hasTestCoverage(ctx context.Context) error {
	files, err := b.c.GitHub.ListFiles(ctx,
		b.c.Environment.Organization,
		b.c.Environment.Repository,
		b.c.Environment.Number)
	if err != nil {
		return trace.Wrap(err)
	}

	var code bool
	var tests bool

	for _, file := range files {
		if strings.HasPrefix(file, "vendor/") {
			continue
		}

		if strings.HasSuffix(file, "_test.go") {
			tests = true
		}
		if strings.HasSuffix(file, ".go") {
			code = true
		}
	}

	// Fail if code was added without test coverage.
	if code && !tests {
		return trace.BadParameter("missing test coverage")
	}
	return nil
}
