// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package parser_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/sql/plpgsql/parser"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/datadriven"
)

func TestParseDataDriver(t *testing.T) {
	datadriven.Walk(t, testutils.TestDataPath(t), func(t *testing.T, path string) {
		datadriven.RunTest(t, path, func(t *testing.T, d *datadriven.TestData) string {
			switch d.Cmd {
			case "parse":
				// Check parse.
				fn, err := parser.Parse(d.Input)
				if err != nil {
					if strings.Contains(err.Error(), "unimplemented") {
						return fmt.Sprintf("expected parse error: %v", err)
					}

					d.Fatalf(t, "unexpected parse error: %v", err)
				}
				// TODO(chengxiong): add pretty print round trip test.
				return fn.String()
			case "feature-count":
				fn, err := parser.ParsePlpgCounter(d.Input)
				if err != nil {
					d.Fatalf(t, "unexpected parse error: %v", err)
				}
				return fn.String()
			}
			d.Fatalf(t, "unsupported command: %s", d.Cmd)
			return ""
		})
	})
}
