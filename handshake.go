// Copyright 2015 JPH <jph@hackworth.be>

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cryptoauth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
	"log"
)

type Challenge struct {
	Type                                uint8
	Lookup                              [7]byte
	RequirePacketAuthAndDerivationCount uint16
	Additional                          uint16
}

type Handshake struct {
	Stage                                  uint32
	Challenge                              *Challenge // We use a generic container initially then decode it into appropriate struct later
	Nonce                                  [24]byte   // 24 bytes
	PublicKey                              *[32]byte
	AuthenticatorAndEncryptedTempPublicKey []byte
	Authenticator                          [16]byte // 16 bytes
	TempPublicKey                          [32]byte // 32 bytes
	Data                                   []byte
}

func (h *Handshake) Marshal(peer *Peer) ([]byte, error) {

	var out []byte

	authenticatedAndEncryptedTempPubKey := box.SealAfterPrecomputation(out, peer.TempKeyPair.PublicKey[:], &h.Nonce, peer.Secret)
	//encryptRandomNonce(h.Nonce, peer.TempKeyPair.PublicKey[:], peer.Secret)

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, h.Stage)
	binary.Write(buf, binary.BigEndian, h.Challenge.Type)
	binary.Write(buf, binary.BigEndian, h.Challenge.Lookup)
	binary.Write(buf, binary.BigEndian, h.Challenge.RequirePacketAuthAndDerivationCount)
	binary.Write(buf, binary.BigEndian, h.Challenge.Additional)
	binary.Write(buf, binary.BigEndian, h.Nonce)
	binary.Write(buf, binary.BigEndian, h.PublicKey)
	binary.Write(buf, binary.BigEndian, authenticatedAndEncryptedTempPubKey)

	return buf.Bytes(), nil
}

// Logic to validate if an inbound handshake is correct based
// on the existing state of the peer

func (peer *Peer) validateHandshake(handshake *Handshake, state *State) error {

	var err error

	err = handshake.isDifferentPublicKeyToPeer(peer)
	if err != nil {
		return err
	}

	// The remote peer presents a hashed password for authentication, which we need
	// to compare against our known passwords. Here we return an error if there's a problem
	// with the supplied challenge (we can't find a matching password)

	password, err := state.checkChallenge(handshake.Data[4:16])
	if err != nil {
		return err
	}

	peer.PasswordHash = password.hash
	var NextNonce uint32

	if handshake.Stage < 2 {
		if isEmpty(&peer.PublicKey) || peer.NextNonce == 0 {
			copy(peer.PublicKey[:], handshake.PublicKey[:])
		}
		peer.Secret = computeSharedSecretWithPasswordHash(state.KeyPair.PrivateKey, peer.PublicKey, peer.PasswordHash)
		NextNonce = 2
	} else {
		if peer.Initiator == false {
			return errAuthentication.setInfo("Unecessary additional key packet received")
		}

		peer.Secret = computeSharedSecretWithPasswordHash(peer.TempKeyPair.PrivateKey, peer.PublicKey, peer.PasswordHash)
		NextNonce = 4
	}

	// Decrypting peer's temp public key

	payload := handshake.Data[72:]
	var herTempPublicKey [32]byte

	decrypted, success := box.OpenAfterPrecomputation(handshake.Data[72:], payload, &handshake.Nonce, peer.Secret)
	if success == false {
		peer.Established = false
		return errAuthentication.setInfo("Decryption of temporary public key failed")
	}

	copy(herTempPublicKey[:], decrypted[88:120])

	// Post-decryption checks

	err = handshake.isDuplicateHelloPacket(peer, herTempPublicKey)
	if err != nil {
		return err
	}

	err = handshake.isDuplicateKeyPacket(peer, herTempPublicKey)
	if err != nil {
		return err
	}

	err = handshake.isKeyPacketWithDifferentTemporaryPublicKey(peer, herTempPublicKey)
	if err != nil {
		return err
	}

	err = handshake.isRepeatKeyPacketDuringSetup(peer, NextNonce, herTempPublicKey, state)
	if err != nil {
		return err
	}

	if isEmpty(&peer.PublicKey) == true && isEmpty(handshake.PublicKey) == false {
		copy(peer.PublicKey[:], handshake.PublicKey[:])
	}

	// TODO: handle data as part of handhsake
	if len(handshake.Data) <= 160 {
		if handshake.Challenge.Additional&(1<<15) != 0 {
			return errNone
		}
	} else {
		panic("got here")
	}

	// if NextNonce == 4 {

	// }

	return errUnknown
}

func (peer *Peer) parseHandshake(stage uint32, data []byte) (*Handshake, error) {

	h := new(Handshake)
	h.Challenge = new(Challenge)

	// Store the raw data for quick manipulations later
	copy(h.Data, data)

	if len(data) < 120 {
		return nil, fmt.Errorf("CryptoAuthHandshake header too short")
	}

	r := bytes.NewReader(data)
	binary.Read(r, binary.BigEndian, &h.Stage)
	binary.Read(r, binary.BigEndian, &h.Challenge.Type)
	binary.Read(r, binary.BigEndian, &h.Challenge.Lookup)
	binary.Read(r, binary.BigEndian, &h.Challenge.RequirePacketAuthAndDerivationCount)
	binary.Read(r, binary.BigEndian, &h.Challenge.Additional)
	binary.Read(r, binary.BigEndian, &h.Nonce)
	binary.Read(r, binary.BigEndian, &h.PublicKey)
	binary.Read(r, binary.BigEndian, &h.Authenticator)
	binary.Read(r, binary.BigEndian, &h.TempPublicKey)

	return h, nil
}

func (peer *Peer) newHandshake(msg []byte, isSetup int, state *State) (*Handshake, error) {

	var err error

	h := new(Handshake)
	h.Challenge = new(Challenge)

	h.Stage = peer.NextNonce

	// Generate a new random 24 byte nonce.
	newNonce := make([]byte, 24)
	rand.Read(newNonce)
	copy(h.Nonce[:], newNonce)

	h.PublicKey = state.KeyPair.PublicKey

	if isEmpty(&peer.PasswordHash) == false {
		panic("encryptHandshake: got here")
		h.Challenge.Type = 1
	} else {
		h.Challenge.Type = 0
	}

	h.Challenge.RequirePacketAuthAndDerivationCount |= (1 << 15)
	h.Challenge.Additional &= ^uint16(1 << 15)

	if peer.NextNonce == 0 || peer.NextNonce == 2 {
		if peer.TempKeyPair == nil {
			peer.TempKeyPair, err = createTempKeyPair()
			if err != nil {
				return nil, err
			}
		}
	}

	if peer.NextNonce < 2 {
		peer.Secret = computeSharedSecretWithPasswordHash(state.KeyPair.PrivateKey, peer.PublicKey, peer.PasswordHash)
		peer.Initiator = true
		peer.NextNonce = 1
	} else {
		peer.Secret = computeSharedSecret(state.KeyPair.PrivateKey, peer.TempPublicKey)
		peer.NextNonce = 3
	}

	// Key Packet
	if peer.NextNonce == 2 {
		peer.Secret = computeSharedSecretWithPasswordHash(state.KeyPair.PrivateKey, peer.TempPublicKey, peer.PasswordHash)
	}

	return h, nil

}

func computeSharedSecret(privateKey *[32]byte, herPublicKey [32]byte) (secret *[32]byte) {

	// TODO: check this, is this right way to check for empty [32]byte?
	box.Precompute(secret, &herPublicKey, privateKey)
	return secret
}

func computeSharedSecretWithPasswordHash(privateKey *[32]byte, herPublicKey [32]byte, passwordHash [32]byte) *[32]byte {

	// TODO: check this, is this right way to check for empty [32]byte?

	var computedKey [32]byte
	curve25519.ScalarMult(&computedKey, privateKey, &herPublicKey)

	buff := make([]byte, 64)
	copy(buff[:32], computedKey[:])
	copy(buff[32:64], passwordHash[:])

	secret := sha256.Sum256(buff)

	return &secret
}

func (state *State) checkChallenge(challenge []byte) (*Passwd, error) {
	if challenge[0] != 1 {
		return nil, errAuthentication.setInfo("Invalid authentication type")
	}

	for _, v := range state.Passwords {
		// a := make([]byte, 8)
		b := make([]byte, 12)

		// copy(a, challenge[:])
		copy(b, v.hash[:12])

		if bytes.Compare(challenge, b) == 0 {
			log.Println("getAuth: found matching account")
			return v, nil
		}
	}

	return nil, errAuthentication.setInfo("No matching password found")
}
