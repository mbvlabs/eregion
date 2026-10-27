package cli

import (
	"strings"
	"testing"
)

func TestValidateImagePushParams(t *testing.T) {
	if err := validateImagePushParams(rpcImagePushParams{Repository: "andurel", Tag: "v1.2_3-local"}); err != nil {
		t.Fatalf("valid params refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		params rpcImagePushParams
	}{
		{name: "empty repository", params: rpcImagePushParams{Tag: "v1"}},
		{name: "uppercase repository", params: rpcImagePushParams{Repository: "Andurel", Tag: "v1"}},
		{name: "underscore repository", params: rpcImagePushParams{Repository: "andurel_site", Tag: "v1"}},
		{name: "leading digit repository", params: rpcImagePushParams{Repository: "1andurel", Tag: "v1"}},
		{name: "too long repository", params: rpcImagePushParams{Repository: strings.Repeat("a", 41), Tag: "v1"}},
		{name: "empty tag", params: rpcImagePushParams{Repository: "andurel"}},
		{name: "slash in tag", params: rpcImagePushParams{Repository: "andurel", Tag: "bad/tag"}},
		{name: "space in tag", params: rpcImagePushParams{Repository: "andurel", Tag: "bad tag"}},
		{name: "too long tag", params: rpcImagePushParams{Repository: "andurel", Tag: strings.Repeat("a", 129)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateImagePushParams(test.params); err == nil {
				t.Fatalf("invalid params accepted: %+v", test.params)
			}
		})
	}
}

func TestParseDockerLoadOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "reference", output: "Loaded image: andurel-site:local\n", want: "andurel-site:local"},
		{name: "image ID", output: "Loaded image ID: sha256:" + strings.Repeat("a", 64) + "\r\n", want: "sha256:" + strings.Repeat("a", 64)},
		{name: "last reference", output: "Loaded image: first:local\nLoaded image: second:local\n", want: "second:local"},
		{name: "reference wins over ID", output: "Loaded image ID: sha256:" + strings.Repeat("b", 64) + "\nLoaded image: andurel-site:local\n", want: "andurel-site:local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseDockerLoadOutput(test.output)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
	if _, err := parseDockerLoadOutput("no useful output\n"); err == nil {
		t.Fatal("output without a loaded image was accepted")
	}
}
