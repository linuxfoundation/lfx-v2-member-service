// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQuoteSOSLPhrase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"acme.com", `"acme.com"`},
		{"my-org.io", `"my\-org.io"`},
		{`a}b"c\d`, `"a\}b\"c\\d"`},
		{`?&|!{}[]()^~*:\"'+-`, `"\?\&\|\!\{\}\[\]\(\)\^\~\*\:\\\"\'\+\-"`},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, quoteSOSLPhrase(tc.in), "quoteSOSLPhrase(%q)", tc.in)
	}
}
