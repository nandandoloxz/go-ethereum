// Copyright 2016 The go-ethereum Authors
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

// Contains the Whisper protocol Message element. For formal details please see
// the specs at https://github.com/ethereum/wiki/wiki/Whisper-PoC-1-Protocol-Spec#messages.
// todo: fix the spec link, and move it to doc.go

package whisper05

import (
	"errors"
	"fmt"

	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	crand "crypto/rand"
	"crypto/sha256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"

	"golang.org/x/crypto/pbkdf2"
)

// Options specifies the exact way a message should be wrapped into an Envelope.
type MessageParams struct {
	TTL      uint32
	Src      *ecdsa.PrivateKey
	Dst      *ecdsa.PublicKey
	KeySym   []byte
	Topic    TopicType
	WorkTime uint32
	PoW      float64
	Payload  []byte
	Padding  []byte
}

// SentMessage represents an end-user data packet to transmit through the
// Whisper protocol. These are wrapped into Envelopes that need not be
// understood by intermediate nodes, just forwarded.
type SentMessage struct {
	Raw []byte
}

// ReceivedMessage represents a data packet to be received through the
// Whisper protocol.
type ReceivedMessage struct {
	Raw []byte

	Payload   []byte
	Padding   []byte
	Signature []byte

	PoW   float64          // Proof of work as described in the Whisper spec
	Sent  uint32           // Time when the message was posted into the network
	TTL   uint32           // Maximum time to live allowed for the message
	Src   *ecdsa.PublicKey // Message recipient (identity used to decode the message)
	Dst   *ecdsa.PublicKey // Message recipient (identity used to decode the message)
	Topic TopicType

	TopicKeyHash    common.Hash // The Keccak256Hash of the key, associated with the Topic
	EnvelopeHash    common.Hash // Message envelope hash to act as a unique id
	EnvelopeVersion uint64
}

func isMessageSigned(flags byte) bool {
	return (flags & signatureFlag) != 0
}

func isMessagePadded(flags byte) bool {
	return (flags & paddingMask) != 0
}

func (self *ReceivedMessage) isSymmetricEncryption() bool {
	return self.TopicKeyHash != common.Hash{}
}

func (self *ReceivedMessage) isAsymmetricEncryption() bool {
	return self.Dst != nil
}

func DeriveOneTimeKey(key []byte, salt []byte, version uint64) (derivedKey []byte, err error) {
	if version == 0 {
		derivedKey = pbkdf2.Key(key, salt, 16, aesKeyLength, sha256.New)
	} else {
		err = fmt.Errorf("DeriveKey: invalid envelope version: %d", version)
	}
	return
}

// NewMessage creates and initializes a non-signed, non-encrypted Whisper message.
func NewSentMessage(params *MessageParams) *SentMessage {
	// Construct an initial flag set: no signature, no padding, other bits random
	buf := make([]byte, 1)
	crand.Read(buf)
	flags := buf[0]
	flags &= ^paddingMask
	flags &= ^signatureFlag

	msg := SentMessage{}
	msg.Raw = make([]byte, 1, len(params.Payload)+len(params.Payload)+signatureLength+padSizeLimitUpper)
	msg.Raw[0] = flags
	msg.appendPadding(params)
	msg.Raw = append(msg.Raw, params.Payload...)
	return &msg
}

// appendPadding appends the pseudorandom padding bytes and sets the padding flag.
// The last byte contains the size of padding (thus, its size must not exceed 256).
func (self *SentMessage) appendPadding(params *MessageParams) {
	total := len(params.Payload) + 1
	if params.Src != nil {
		total += signatureLength
	}
	padChunk := padSizeLimitUpper
	if total <= padSizeLimitLower {
		padChunk = padSizeLimitLower
	}
	odd := total % padChunk
	if odd > 0 {
		padSize := padChunk - odd
		if padSize > 255 {
			// this algorithm is only valid if padSizeLimitUpper <= 256.
			// if padSizeLimitUpper will every change, please fix the algorithm
			// (for more information see ReceivedMessage.extractPadding() function).
			panic("please fix the padding algorithm before releasing new version")
		}
		buf := make([]byte, padSize)
		crand.Read(buf[1:])
		buf[0] = byte(padSize)
		if params.Padding != nil {
			copy(buf[1:], params.Padding)
		}
		self.Raw = append(self.Raw, buf...)
		self.Raw[0] |= byte(0x1) // number of bytes indicating the padding size
	}
}

// sign calculates and sets the cryptographic signature for the message,
// also setting the sign flag.
func (self *SentMessage) sign(key *ecdsa.PrivateKey) (err error) {
	if isMessageSigned(self.Raw[0]) {
		// this should not happen, but no reason to panic
		glog.V(logger.Error).Infof("Trying to sign a message which was already signed")
		return
	}
	hash := crypto.Keccak256(self.Raw)
	signature, err := crypto.Sign(hash, key)
	if err != nil {
		self.Raw = append(self.Raw, signature...)
		self.Raw[0] |= signatureFlag
	}
	return
}

// encryptAsymmetric encrypts a message with a public key.
func (self *SentMessage) encryptAsymmetric(key *ecdsa.PublicKey) error {
	if !validatePublicKey(key) {
		return fmt.Errorf("Invalid public key provided for asymmetric encryption")
	}
	encrypted, err := crypto.Encrypt(key, self.Raw)
	if err == nil {
		self.Raw = encrypted
	}
	return err
}

// encryptSymmetric encrypts a message with a topic key, using AES-GCM-256.
// nonce size should be 12 bytes (see cipher.gcmStandardNonceSize).
func (self *SentMessage) encryptSymmetric(key []byte) (salt []byte, nonce []byte, err error) {
	if !validateSymmetricKey(key) {
		err = fmt.Errorf("encryptSymmetric: invalid key provided for symmetric encryption")
		return
	}

	salt = make([]byte, saltLength)
	_, err = crand.Read(salt)
	if err != nil {
		return
	} else if !validateSymmetricKey(salt) {
		err = fmt.Errorf("encryptSymmetric: failed to generate salt")
		return
	}

	derivedKey, err := DeriveOneTimeKey(key, salt, EnvelopeVersion)
	if err != nil {
		return
	}
	if !validateSymmetricKey(derivedKey) {
		err = fmt.Errorf("encryptSymmetric: invalid key derived")
		return
	}
	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return
	}

	// never use more than 2^32 random nonces with a given key
	nonce = make([]byte, aesgcm.NonceSize())
	_, err = crand.Read(nonce)
	if err != nil {
		return
	}
	self.Raw = aesgcm.Seal(nil, nonce, self.Raw, nil)
	return
}

// Wrap bundles the message into an Envelope to transmit over the network.
//
// pow (Proof Of Work) controls how much time to spend on hashing the message,
// inherently controlling its priority through the network (smaller hash, bigger
// priority).
//
// The user can control the amount of identity, privacy and encryption through
// the options parameter as follows:
//   - options.From == nil && options.To == nil: anonymous broadcast
//   - options.From != nil && options.To == nil: signed broadcast (known sender)
//   - options.From == nil && options.To != nil: encrypted anonymous message
//   - options.From != nil && options.To != nil: encrypted signed message
func (self *SentMessage) Wrap(options MessageParams) (envelope *Envelope, err error) {
	if options.TTL == 0 {
		options.TTL = DefaultTTL
	}
	if options.Src != nil {
		if err = self.sign(options.Src); err != nil {
			return
		}
	}
	if len(self.Raw) > msgMaxLength {
		glog.V(logger.Error).Infof("Message size must not exceed %d bytes", msgMaxLength)
		err = errors.New("Oversized message")
		return
	}
	var salt, nonce []byte
	if options.Dst != nil {
		err = self.encryptAsymmetric(options.Dst)
	} else if options.KeySym != nil {
		salt, nonce, err = self.encryptSymmetric(options.KeySym)
	} else {
		err = errors.New("Unable to encrypt the message: neither Dst nor Key")
	}

	if err == nil {
		envelope = NewEnvelope(options.TTL, options.Topic, salt, nonce, self)
		envelope.Seal(options)
	}
	return
}

// decryptSymmetric decrypts a message with a topic key, using AES-GCM-256.
// nonce size should be 12 bytes (see cipher.gcmStandardNonceSize).
func (self *ReceivedMessage) decryptSymmetric(key []byte, salt []byte, nonce []byte) error {
	derivedKey, err := DeriveOneTimeKey(key, salt, self.EnvelopeVersion)
	if err != nil {
		return err
	}

	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if len(nonce) != aesgcm.NonceSize() {
		glog.V(logger.Error).Infof("AES nonce size must be %d bytes", aesgcm.NonceSize())
		return errors.New("Wrong AES nonce size")
	}
	decrypted, err := aesgcm.Open(nil, nonce, self.Raw, nil)
	if err != nil {
		return err
	}
	self.Raw = decrypted
	return nil
}

// decryptAsymmetric decrypts an encrypted payload with a private key.
func (self *ReceivedMessage) decryptAsymmetric(key *ecdsa.PrivateKey) error {
	decrypted, err := crypto.Decrypt(key, self.Raw)
	if err == nil {
		self.Raw = decrypted
	}
	return err
}

// Validate checks the validity and extracts the fields in case of success
func (self *ReceivedMessage) Validate() bool {
	end := len(self.Raw)
	if end < 1 {
		return false
	}

	if isMessageSigned(self.Raw[0]) {
		end -= signatureLength
		if end <= 1 {
			return false
		}
		self.Signature = self.Raw[end:]
		self.Src = self.Recover()
		if self.Src == nil {
			return false
		}
	}

	padSize, ok := self.extractPadding(end)
	if !ok {
		return false
	}

	self.Payload = self.Raw[1+padSize : end]
	return self.isSymmetricEncryption() != self.isAsymmetricEncryption()
}

// extractPadding extracts the padding from raw message.
// although we don't support sending messages with padding size
// exceeding 255 bytes, such messages are perfectly valid, and
// can be successfully decrypted.
func (self *ReceivedMessage) extractPadding(end int) (int, bool) {
	paddingSize := 0
	sz := int(self.Raw[0] & paddingMask) // number of bytes containing the entire size of padding, could be zero
	if sz != 0 {
		paddingSize = int(bytesToIntLittleEndian(self.Raw[1 : 1+sz]))
		if paddingSize < sz || paddingSize+1 > end {
			return 0, false
		}
		self.Padding = self.Raw[1+sz : 1+paddingSize]
	}
	return paddingSize, true
}

// Recover retrieves the public key of the message signer.
func (self *ReceivedMessage) Recover() *ecdsa.PublicKey {
	defer func() { recover() }() // in case of invalid signature

	pub, err := crypto.SigToPub(self.hash(), self.Signature)
	if err != nil {
		glog.V(logger.Error).Infof("Could not get public key from signature: %v", err)
		return nil
	}
	return pub
}

// hash calculates the SHA3 checksum of the message flags, payload and padding.
func (self *ReceivedMessage) hash() []byte {
	if isMessageSigned(self.Raw[0]) {
		sz := len(self.Raw) - signatureLength
		return crypto.Keccak256(self.Raw[:sz])
	}
	return crypto.Keccak256(self.Raw)
}
