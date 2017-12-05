// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package discover implements the Ethereum Node Record as per https://github.com/ethereum/EIPs/pull/778
package enr

import (
	"bytes"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"

	"github.com/btcsuite/btcd/btcec"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/sha3"
	"github.com/ethereum/go-ethereum/rlp"
)

var (
	errNoID           = errors.New("unknown or unspecified identity scheme")
	errInvalidSigsize = errors.New("invalid signature size")
	errInvalidSig     = errors.New("invalid signature")
	errNotSorted      = errors.New("record key/value pairs are not sorted by key")
	errDuplicateKey   = errors.New("record contains duplicate key")
	errIncompletePair = errors.New("record contains incomplete k/v pair")
)

// Key is implemented by known node record key types.
//
// To define a new key that is to be included in a node record,
// create a Go type that satisfies this interface. The type should
// also implement rlp.Decoder if additional checks are needed on the value.
type Key interface {
	ENRKey() string
}

// pair is a key/value pair in a record.
type pair struct {
	k string
	v rlp.RawValue
}

type Record struct {
	seq       uint32 // sequence number
	signature []byte // record's signature
	raw       []byte // RLP encoded record
	pairs     []pair // sorted list of all key/value pairs
}

func (r Record) Seq() uint32 {
	return r.seq
}

func (r *Record) SetSeq(s uint32) {
	r.signature = nil
	r.seq = s
}

func (r *Record) Load(k Key) (bool, error) {
	i := sort.Search(len(r.pairs), func(i int) bool { return r.pairs[i].k >= k.ENRKey() })

	if i < len(r.pairs) && r.pairs[i].k == k.ENRKey() {
		return true, rlp.DecodeBytes(r.pairs[i].v, k)
	}

	return false, errors.New("record does not exist")
}

func (r *Record) Set(k Key) error {
	r.signature = nil
	blob, err := rlp.EncodeToBytes(k)
	if err != nil {
		return err
	}
	var inserted bool
	for i, p := range r.pairs {
		if p.k == k.ENRKey() {
			// replace value of pair
			r.pairs[i].v = blob
			inserted = true

			break
		} else if p.k > k.ENRKey() {
			// insert pair before i-th elem
			el := pair{k.ENRKey(), blob}

			r.pairs = append(r.pairs, pair{})
			copy(r.pairs[i+1:], r.pairs[i:])
			r.pairs[i] = el

			inserted = true
			break
		}
	}
	if !inserted {
		r.pairs = append(r.pairs, pair{k.ENRKey(), blob})
	}
	return nil
}

func (r Record) EncodeRLP(w io.Writer) error {
	if r.signature == nil {
		return errors.New("record is not signed")
	}
	_, err := w.Write(r.raw)
	return err
}

func (r *Record) DecodeRLP(s *rlp.Stream) error {
	raw, err := s.Raw()
	if err != nil {
		return err
	}

	// Decode the RLP container.
	dec := Record{raw: raw}
	s = rlp.NewStream(bytes.NewReader(raw), 0)
	if _, err := s.List(); err != nil {
		return err
	}
	if err = s.Decode(&dec.signature); err != nil {
		return err
	}
	if err = s.Decode(&dec.seq); err != nil {
		return err
	}
	// The rest of the record contains sorted k/v pairs.
	var prevkey string
	for i := 0; ; i++ {
		var kv pair
		if err := s.Decode(&kv.k); err != nil {
			if err == rlp.EOL {
				break
			}
			return err
		}
		if err := s.Decode(&kv.v); err != nil {
			if err == rlp.EOL {
				return errIncompletePair
			}
			return err
		}
		if i > 0 {
			if kv.k == prevkey {
				return errDuplicateKey
			}
			if kv.k < prevkey {
				return errNotSorted
			}
		}
		dec.pairs = append(dec.pairs, kv)
		prevkey = kv.k
	}
	if err := s.ListEnd(); err != nil {
		return err
	}

	// Verify signature.
	if err = dec.verifySignature(); err != nil {
		return err
	}
	*r = dec
	return nil
}

func (r *Record) NodeAddr() ([]byte, error) {
	var secp256k1 Secp256k1

	_, err := r.Load(&secp256k1)
	if err != nil {
		return nil, err
	}

	pk := btcec.PublicKey(secp256k1)

	digest := crypto.Keccak256Hash(pk.SerializeCompressed())

	return digest.Bytes(), nil
}

func (r *Record) Sign(privkey *ecdsa.PrivateKey) error {
	pk := (*btcec.PublicKey)(&privkey.PublicKey)
	r.seq = r.seq + 1
	r.Set(ID(ID_SECP256k1_KECCAK))
	r.Set(Secp256k1(*pk))
	return r.signAndEncode(privkey)
}

func (r *Record) appendPairs(list []interface{}) []interface{} {
	list = append(list, r.seq)
	for _, p := range r.pairs {
		list = append(list, p.k, p.v)
	}
	return list
}

func (r *Record) signAndEncode(privkey *ecdsa.PrivateKey) error {
	// Put record elements into a flat list. Leave room for the signature.
	list := make([]interface{}, 1, len(r.pairs)*2+2)
	list = r.appendPairs(list)

	// Sign the tail of the list.
	h := sha3.NewKeccak256()
	rlp.Encode(h, list[1:])
	sig, err := (*btcec.PrivateKey)(privkey).Sign(h.Sum(nil))
	if err != nil {
		return err
	}

	// Put signature in front.
	r.signature = encodeCompactSignature(sig)
	list[0] = r.signature
	r.raw, _ = rlp.EncodeToBytes(list)
	return nil
}

func (r *Record) verifySignature() error {
	// Get identity scheme, public key, signature.
	var id ID
	var secp256k1 Secp256k1
	if _, err := r.Load(&id); err != nil {
		return err
	} else if id != ID_SECP256k1_KECCAK {
		return errNoID
	}
	if ok, err := r.Load(&secp256k1); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("can't verify signature: missing %q key", secp256k1.ENRKey())
	}
	sig, err := parseCompactSignature(r.signature)
	if err != nil {
		return err
	}

	// Verify the signature.
	list := make([]interface{}, 0, len(r.pairs)*2+1)
	list = r.appendPairs(list)
	h := sha3.NewKeccak256()
	rlp.Encode(h, list)
	if !sig.Verify(h.Sum(nil), (*btcec.PublicKey)(&secp256k1)) {
		return errInvalidSig
	}
	return nil
}

func encodeCompactSignature(sig *btcec.Signature) []byte {
	b := make([]byte, 64)
	math.ReadBits(sig.R, b[:32])
	math.ReadBits(sig.S, b[32:])
	return b
}

func parseCompactSignature(sig []byte) (*btcec.Signature, error) {
	if len(sig) != 64 {
		return nil, errInvalidSigsize
	}
	return &btcec.Signature{R: new(big.Int).SetBytes(sig[:32]), S: new(big.Int).SetBytes(sig[32:])}, nil
}
