// Package bundle is the transfer bundle of the secrets handoff, as in vaire:
// the record encrypted with age to an 8-word passphrase and ASCII-armored, so
// it can sit in a password manager and be pasted into eregion login.
package bundle

import (
	"bufio"
	"bytes"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// eff_large_wordlist.txt is the EFF large diceware list (7776 words), kept
// verbatim.
//
//go:embed eff_large_wordlist.txt
var wordlistRaw string

// Words is the passphrase length: 8 words, about 103 bits.
const Words = 8

// scryptWorkFactor is the age scrypt work factor, as in vaire.
const scryptWorkFactor = 18

// maxPlain caps the decrypted record.
const maxPlain = 4 << 20

// Header and Footer delimit an armored bundle.
const (
	Header = armor.Header
	Footer = armor.Footer
)

var (
	wordsOnce sync.Once
	words     []string
)

func wordlist() []string {
	wordsOnce.Do(func() {
		for _, line := range strings.Split(wordlistRaw, "\n") {
			// Words are joined with hyphens, so the four hyphenated entries
			// (drop-down, felt-tip, t-shirt, yo-yo) are left out.
			if fields := strings.Fields(line); len(fields) == 2 && !strings.Contains(fields[1], "-") {
				words = append(words, fields[1])
			}
		}
		if len(words) != 7772 {
			panic("embedded EFF wordlist is damaged")
		}
	})
	return words
}

// NewPassphrase returns 8 EFF words joined with "-".
func NewPassphrase() (string, error) {
	list := wordlist()
	limit := big.NewInt(int64(len(list)))
	out := make([]string, Words)
	for i := range out {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out[i] = list[n.Int64()]
	}
	return strings.Join(out, "-"), nil
}

// Encrypt returns plain as an armored age file for passphrase.
func Encrypt(plain []byte, passphrase string) ([]byte, error) {
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	recipient.SetWorkFactor(scryptWorkFactor)
	var out bytes.Buffer
	aw := armor.NewWriter(&out)
	w, err := age.Encrypt(aw, recipient)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ErrPassphrase is a passphrase that does not open the bundle.
var ErrPassphrase = errors.New("the passphrase does not open this transfer bundle")

// Decrypt opens an armored bundle with passphrase.
func Decrypt(armored []byte, passphrase string) ([]byte, error) {
	identity, err := age.NewScryptIdentity(strings.TrimSpace(passphrase))
	if err != nil {
		return nil, fmt.Errorf("the passphrase is empty")
	}
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(bytes.TrimSpace(armored))), identity)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, ErrPassphrase
		}
		return nil, fmt.Errorf("the transfer bundle is damaged: %w", err)
	}
	plain, err := io.ReadAll(io.LimitReader(r, maxPlain))
	if err != nil {
		return nil, ErrPassphrase
	}
	return plain, nil
}

// Read returns the armored block from r, from its header line to its footer
// line; whatever precedes it, such as the copied record, is skipped. It stops
// at the footer, so a paste needs no Ctrl-D.
func Read(r io.Reader) ([]byte, error) {
	var b strings.Builder
	sc := bufio.NewScanner(io.LimitReader(r, maxPlain))
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if b.Len() == 0 && line != Header {
			continue
		}
		b.WriteString(line + "\n")
		if line == Footer {
			return []byte(b.String()), nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if b.Len() == 0 {
		return nil, fmt.Errorf("no transfer bundle (%s … %s) found", Header, Footer)
	}
	return nil, fmt.Errorf("the transfer bundle ends before %s", Footer)
}
