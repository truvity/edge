// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

// Package chart_test holds static-site's identity check to the account it is
// told to expect. The id is built at run time: this repository is public and
// its leak canary rejects a literal 12-digit run.
package chart_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const identityValues = `
registry:
  auth: ecr
  ack:
    enabled: true
    clusterName: example
    repositoryArns:
      - arn:aws:ecr:eu-central-1:%[1]s:repository/example
%[2]s
site:
  artifact:
    ref: registry.example.com/static/site
serviceAccount:
  name: static-site
`

func render(t *testing.T, values string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "helm", "template", "x", ".", "--namespace", "ns", "-f", "-")
	cmd.Stdin = strings.NewReader(values)
	out, err := cmd.CombinedOutput()

	return string(out), err
}

func checkArgs(t *testing.T, rendered string) []any {
	t.Helper()

	for _, d := range strings.Split(rendered, "\n---") {
		var m map[string]any
		if yaml.Unmarshal([]byte(d), &m) != nil || m["kind"] != "Job" {
			continue
		}

		spec := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)

		args, _ := spec["containers"].([]any)[0].(map[string]any)["args"].([]any) // absent when nothing is asserted

		return args
	}

	t.Fatalf("no identity check Job in:\n%s", rendered)

	return nil
}

func TestIdentityCheckExpectsTheAccount(t *testing.T) {
	account := strings.Repeat("7", 12)

	out, err := render(t, fmt.Sprintf(identityValues, account, "    accountId: \""+account+"\""))
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	args := checkArgs(t, out)
	want := []string{"-expect-account", account}

	if len(args) < 2 || args[len(args)-2] != want[0] || args[len(args)-1] != want[1] {
		t.Fatalf("args = %v, want them to end with %v", args, want)
	}
}

func TestIdentityCheckWithoutAccountPassesNoFlag(t *testing.T) {
	out, err := render(t, fmt.Sprintf(identityValues, strings.Repeat("7", 12), ""))
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	for _, a := range checkArgs(t, out) {
		if a == "-expect-account" {
			t.Fatalf("no accountId was set, but the check passes -expect-account")
		}
	}
}

func TestIdentityCheckRefusesAShortAccount(t *testing.T) {
	if out, err := render(t, fmt.Sprintf(identityValues, strings.Repeat("7", 12), "    accountId: \"12345\"")); err == nil {
		t.Fatalf("a short accountId rendered:\n%s", out)
	}
}
