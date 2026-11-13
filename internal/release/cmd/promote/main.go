// Command promote conditionally advances a signed release pointer.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/mbvlabs/eregion/internal/release"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	bucket := fs.String("bucket", "", "release bucket")
	key := fs.String("key", "", "pointer key")
	endpoint := fs.String("endpoint", "", "S3 endpoint")
	channel := fs.String("channel", "", "logical channel or internal major partition")
	root := fs.String("root", "", "signed release root")
	version := fs.String("version", "", "candidate version")
	commit := fs.String("commit", "", "trusted workflow checkout commit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bucket == "" || *key == "" || *endpoint == "" || *channel == "" || *root == "" || *version == "" || *commit == "" || fs.NArg() != 0 {
		return fmt.Errorf("bucket, key, endpoint, channel, root, version and commit are required")
	}
	expectedKey := map[string]string{"stable": "manifest.json", "edge": "edge/manifest.json", "rc": "rc/manifest.json"}[*channel]
	if expectedKey == "" {
		v, ok := release.ParseStable(*version)
		if !ok || *channel != v.Channel() || v.Prerelease() {
			return fmt.Errorf("invalid channel partition")
		}
		expectedKey = v.Line() + "/manifest.json"
	}
	if *key != expectedKey {
		return fmt.Errorf("channel and pointer key do not match")
	}
	store, candidate, cleanup, err := newProductionStore(ctx, *bucket, *key, *endpoint, *channel, *root, *version, *commit)
	if err != nil {
		return err
	}
	defer cleanup()
	moved, err := advance(ctx, store, func(current pointer) (bool, error) {
		switch *channel {
		case "edge":
			return edgePromotes(current, candidate, gitAncestor)
		case "rc":
			return release.PromotesRC(current.version, candidate.version)
		default:
			return release.Promotes(current.version, candidate.version)
		}
	})
	if err != nil {
		return err
	}
	if moved {
		// Compatibility files are separate objects; old clients can observe a
		// mismatched signature during concurrent publication and fail closed.
		if err := store.publishCompatibility(ctx); err != nil {
			return err
		}
		fmt.Println("promoted")
	} else {
		fmt.Println("unchanged")
	}
	return nil
}
