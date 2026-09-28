// vineyard-sign writes a release signature (<binary>.sig) for each vineyardd build it is given, with
// the keys in VINEYARD_SIGNING_KEY_A and VINEYARD_SIGNING_KEY_B. The release workflow runs it; it is
// not part of the daemon.
//
//	vineyard-sign -version v0.3.23 bin/vineyardd-*
//	vineyard-sign -verify bin/vineyardd-*    (check each build against its .sig and the compiled-in keys)
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/release"
)

var nameRe = regexp.MustCompile(`^vineyardd-([a-z0-9]+)-([a-z0-9]+)(\.exe)?$`)

func main() {
	version := flag.String("version", "", "the version the binaries report (vineyardd version)")
	require2 := flag.Bool("require-both", true, "fail unless both signing keys are set")
	verify := flag.Bool("verify", false, "only check each binary against the .sig beside it")
	flag.Parse()
	if *verify {
		os.Exit(verifyAll(flag.Args()))
	}
	if *version == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: vineyard-sign -version VERSION BINARY...")
		os.Exit(2)
	}
	keys, err := release.KeysFromEnv()
	if err != nil {
		fail(err)
	}
	if *require2 && len(keys) < 2 {
		fail(fmt.Errorf("only %d signing key set; a release is signed with both", len(keys)))
	}
	for _, bin := range flag.Args() {
		if strings.HasSuffix(bin, ".sig") {
			continue
		}
		m := nameRe.FindStringSubmatch(filepath.Base(bin))
		if m == nil {
			fail(fmt.Errorf("%s: not a vineyardd-<os>-<arch> build", bin))
		}
		data, err := os.ReadFile(bin)
		if err != nil {
			fail(err)
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		sig := release.Sign(m[1]+"-"+m[2], *version, sha, keys)
		out, _ := json.MarshalIndent(sig, "", "  ")
		if err := os.WriteFile(release.SigPath(bin), append(out, '\n'), 0o644); err != nil {
			fail(err)
		}
		if err := release.Verify(out, m[1]+"-"+m[2], *version, sha); err != nil {
			fail(fmt.Errorf("%s: the signature just written does not verify against the compiled-in keys: %w", bin, err))
		}
		fmt.Printf("signed %s (%s %s, %.12s)\n", bin, m[1]+"-"+m[2], *version, sha)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "vineyard-sign:", err)
	os.Exit(1)
}

// verifyAll checks that every binary's .sig is valid for exactly that file, so a build replaced after
// signing (a packaging step that rebuilt it, say) fails the release instead of shipping.
func verifyAll(bins []string) int {
	bad := 0
	for _, bin := range bins {
		if strings.HasSuffix(bin, ".sig") {
			continue
		}
		m := nameRe.FindStringSubmatch(filepath.Base(bin))
		if m == nil {
			fmt.Fprintf(os.Stderr, "%s: not a vineyardd-<os>-<arch> build\n", bin)
			bad++
			continue
		}
		data, err := os.ReadFile(bin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			bad++
			continue
		}
		sum := sha256.Sum256(data)
		var sig release.Signature
		raw := release.ReadSig(bin)
		_ = json.Unmarshal(raw, &sig)
		if err := release.Verify(raw, m[1]+"-"+m[2], sig.Version, hex.EncodeToString(sum[:])); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", bin, err)
			bad++
			continue
		}
		fmt.Printf("ok %s (%s %s)\n", bin, m[1]+"-"+m[2], sig.Version)
	}
	if bad > 0 {
		return 1
	}
	return 0
}
