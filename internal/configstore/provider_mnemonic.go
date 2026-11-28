package configstore

import (
	"errors"
	"strings"

	"github.com/tyler-smith/go-bip39"
)

/* The recovery phrase, and nothing else.
 *
 * A key's ONLY root is its 24 words: the Main Key is derived from them
 * (deriveMainKey), so exporting the phrase and using this machine are the same
 * ability. That is why the phrase is not a provider here any more — a "mnemonic
 * slot" would seal the Main Key under a KEK derived from the very words that
 * already determine it, which is a wrapper around nothing.
 *
 * What remains is the BIP39 codec: generating words, and checking that the words
 * a person typed are words a person could have written down.
 */

// mnemonicStrength is 256 bits, i.e. 24 words. A key is the one secret in this
// system that must survive a house move, so it gets the full width rather than
// BIP39's 128-bit default.
const mnemonicStrength = 256

// ErrMnemonicInvalid reports a phrase that is not a valid BIP39 mnemonic.
var ErrMnemonicInvalid = errors.New("configstore: the recovery phrase is not valid")

// NewMnemonic generates a fresh 24-word phrase.
func NewMnemonic() (string, error) {
	entropy, err := bip39.NewEntropy(mnemonicStrength)
	if err != nil {
		return "", err
	}
	return bip39.NewMnemonic(entropy)
}

// NormalizeMnemonic checks a phrase and returns its canonical spelling: lower
// case, single ASCII spaces, no surrounding whitespace. Users type these by hand
// off paper, so casing and stray whitespace are forgiven; a wrong word is not,
// because a single wrong word is a different key.
func NormalizeMnemonic(phrase string) (string, error) {
	phrase = strings.ToLower(collapseSpaces(phrase))
	if phrase == "" || len(phrase) > maxMnemonicBytes {
		return "", ErrMnemonicInvalid
	}
	if !bip39.IsMnemonicValid(phrase) {
		return "", ErrMnemonicInvalid
	}
	return phrase, nil
}

// collapseSpaces trims and single-spaces a phrase typed off paper.
func collapseSpaces(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
