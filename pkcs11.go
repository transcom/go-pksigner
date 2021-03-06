/* {{{ Copyright (c) 2017, Paul R. Tagliamonte <paultag@gmail.com>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in
 * all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
 * THE SOFTWARE. }}} */

package pksigner

import (
	"crypto"
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	"io"
	"math/big"
	"strings"

	"pault.ag/go/piv"

	"github.com/miekg/pkcs11"
)

// When signing things via PKCS11 directly, raw, we have to handle the hash
// prefixes by manually prefixing the hash with the following byte strings.
// We do this as part of our Sign interface, and try to make this easier for
// our user(s), but this will result in an invalid signature if the crypto.Hash
// isn't in this list, so this requires a lot of caring and feeding.
var hashOIDs = map[crypto.Hash][]byte{
	crypto.SHA224: {0x30, 0x2d, 0x30, 0x0d, 0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x04, 0x05, 0x00, 0x04, 0x1c},
	crypto.SHA256: {0x30, 0x31, 0x30, 0x0d, 0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x01, 0x05, 0x00, 0x04, 0x20},
	crypto.SHA384: {0x30, 0x41, 0x30, 0x0d, 0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x02, 0x05, 0x00, 0x04, 0x30},
	crypto.SHA512: {0x30, 0x51, 0x30, 0x0d, 0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x03, 0x05, 0x00, 0x04, 0x40},
}

// Config is the HSM Configuration object, setting which PKCS#11 module to use,
// Certificate and Private Key strings, a PIN (if needed), and the label of the
// token.
type Config struct {
	// Full path to the PKCS#11 object on the filesystem. The exact value
	// of this depends on the host, but should usually end in a .so
	Module string

	// Label of the x.509 Certificate stored on the PKCS#11 token,
	// for PIV devices, one option for this might be
	// "Certificate for PIV Authentication"
	CertificateLabel string

	// Private key material backing the x.509 Certificate indicated by
	// CertificateLabel, one option for this might be "PIV AUTH key"
	PrivateKeyLabel string

	// Name of the token, if there are more than exactly one.
	TokenLabel string
}

// GetCertificateTemplate creates a pkcs11.Attribute array containing
// constraints that should uniquely identify the PKCS#11 Certificate we're
// interested in
func (c Config) GetCertificateTemplate() []*pkcs11.Attribute {
	return []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, c.CertificateLabel),
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_CERTIFICATE),
	}
}

// GetPrivateKeyTemplate returns a pkcs11.Attribute array containing
// constraints that should uniquely identify the PKCS#11 private key we're
// interested in
func (c Config) GetPrivateKeyTemplate() []*pkcs11.Attribute {
	return []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, c.PrivateKeyLabel),
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_PRIVATE_KEY),
	}
}

// Figure out if the TokenInfo we're looking for matches the TokenInfo
// we've got in front of us. This is used to filter out tokens during
// the setup phase.
func (c Config) slotMatchesCriteria(tokenInfo pkcs11.TokenInfo) bool {
	return strings.TrimRight(tokenInfo.Label, "\x00") == c.TokenLabel
}

// SelectSlot takes a pkcs11.Ctx and a list of slots, figures out which slot is
// the slot we're interested in, returning an error if there's nothing we
// should be using.
func (c Config) SelectSlot(context *pkcs11.Ctx, slots []uint) (uint, error) {
	/* If there's no label matching, and there's only one slot, return
	 * that slot */
	if c.TokenLabel == "" {
		if len(slots) == 1 {
			return slots[0], nil
		}
		// return nil, fmt.Errorf  ???
	}

	for _, slot := range slots {
		token, err := context.GetTokenInfo(slot)
		if err != nil {
			return 0, err
		}
		if c.slotMatchesCriteria(token) {
			return slot, nil
		}
	}
	return 0, fmt.Errorf("No matching slot found")
}

// Close logs out of the Token, and closes any open sessions we might
// have open. This method ought to be defer'd after creating a new
// hsm.Store.
func (s Store) Close() error {
	if s.loggedIn {
		if s.context != nil && s.session != nil {
			if err := s.context.Logout(*s.session); err != nil {
				return err
			}
		}
	}

	if s.session != nil {
		return s.context.CloseSession(*s.session)
	}

	if s.context != nil {
		s.context.Destroy()
		if err := s.context.Finalize(); err != nil {
			return err
		}
	}

	return nil
}

// New creates a new hsm.Store defined by the hsm.Config. If no slot can be
// found, or the underlying infrastructure throws a problem at us, we will
// return an error.
func New(config Config) (*Store, error) {
	cStore := Store{config: &config}

	cStore.context = pkcs11.New(config.Module)
	if err := cStore.context.Initialize(); err != nil {
		return nil, err
	}

	slots, err := cStore.context.GetSlotList(true)
	if err != nil {
		return nil, err
	}

	slot, err := config.SelectSlot(cStore.context, slots)
	if err != nil {
		return nil, err
	}

	// XXX: only get rw if it's needed
	var sessionBitmask uint = pkcs11.CKF_SERIAL_SESSION // | pkcs11.CKF_RW_SESSION
	session, err := cStore.context.OpenSession(slot, sessionBitmask)
	if err != nil {
		return nil, err
	}
	cStore.session = &session

	cert, err := cStore.LoadCertificate()
	if err != nil {
		return nil, err
	}

	cStore.Certificate = cert
	cStore.PublicKey = cert.PublicKey

	return &cStore, err
}

// Login unlocks a smartcard with the provided PIN.
func (s Store) Login(pin string) error {
	err := s.context.Login(*s.session, pkcs11.CKU_USER, pin)
	if err == nil {
		s.loggedIn = true
	}
	return err
}

// Store is the internal hsm.Store encaupsulating state. This implements the
// store.Store interface, as well as crypto.Signer, and crypto.Decryptor.
type Store struct {
	config *Config

	session *pkcs11.SessionHandle
	context *pkcs11.Ctx

	loggedIn bool

	PublicKey   crypto.PublicKey
	Certificate *piv.Certificate
}

// Get the object handles that match the set of pkcs11.Attribute critiera
func (s Store) getObjectHandles(template []*pkcs11.Attribute) ([]pkcs11.ObjectHandle, error) {
	if err := s.context.FindObjectsInit(*s.session, template); err != nil {
		return nil, err
	}
	objects := []pkcs11.ObjectHandle{}
	for {
		obj, more, err := s.context.FindObjects(*s.session, 8)
		if err != nil {
			return nil, err
		}
		objects = append(objects, obj...)

		if !more {
			break
		}
	}
	if err := s.context.FindObjectsFinal(*s.session); err != nil {
		return nil, err
	}
	return objects, nil
}

// Get the one and only one object that match the set of pkcs11.Attribute
// criteria. If multiple handles are returned, throw an error out,
// and if no objects are returned, throw an error.
func (s Store) getObjectHandle(template []*pkcs11.Attribute) (*pkcs11.ObjectHandle, error) {
	candidates, err := s.getObjectHandles(template)
	if err != nil {
		return nil, err
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("pkcs11: the token has no such object")
	} else if len(candidates) != 1 {
		return nil, fmt.Errorf("The query resulted in too many objects")
	}
	return &candidates[0], nil
}

// Find the object defined by `locate`, and return the attributes returned by
// `attributes`. This is useful for looking up an object that we know is
// unique, and returning the attributes we're interested in.
func (s Store) getAttributes(locate, attributes []*pkcs11.Attribute) ([]*pkcs11.Attribute, error) {
	objectHandle, err := s.getObjectHandle(locate)
	if err != nil {
		return nil, err
	}
	return s.context.GetAttributeValue(*s.session, *objectHandle, attributes)
}

// Find the object defined by `locate`, and return the attribute we're interested
// in, defined by `attribuets`. If multiple handles or multiple attribuets are
// returned, an error will be returned.
func (s Store) getAttribute(locate, attributes []*pkcs11.Attribute) (*pkcs11.Attribute, error) {
	attr, err := s.getAttributes(locate, attributes)
	if err != nil {
		return nil, err
	}

	if len(attr) != 1 {
		return nil, fmt.Errorf("The query resulted in too many attributes")
	}

	return attr[0], nil
}

// LoadCertificate queries the underlying HSM Store for the x509 Certificate
// we're interested in, and returns a Go x509.Certificate.
func (s Store) LoadCertificate() (*piv.Certificate, error) {
	certAttribute, err := s.getAttribute(
		s.config.GetCertificateTemplate(),
		[]*pkcs11.Attribute{pkcs11.NewAttribute(pkcs11.CKA_VALUE, nil)},
	)
	if err != nil {
		return nil, err
	}
	return piv.ParseCertificate(certAttribute.Value)
}

// Create a Go rsa.PublicKey from the PKCS#11 attribute array we've fetched from
// the underlying store.
func createPubkeyFromAttributes(attributes []*pkcs11.Attribute) (crypto.PublicKey, error) {
	key := rsa.PublicKey{
		N: big.NewInt(0),
	}

	for _, attribute := range attributes {
		switch attribute.Type {
		case pkcs11.CKA_MODULUS:
			key.N.SetBytes(attribute.Value)
		case pkcs11.CKA_PUBLIC_EXPONENT:
			exp := big.NewInt(0)
			exp.SetBytes(attribute.Value)
			// XXX: Yikes. rsa.PublicKey isn't a BigInt, but the PKCS11 spec
			//      seems to want it to be. Rather than do a special case byte
			//      order decode to int here, I'm going to set it into a BigInt
			//      (in the world that rsa.PublicKey.E turns into a BigInt)
			//      and otherwise just take the few extra cycles of memory
			//      copies. Meh. My sanity isn't worth it.
			key.E = int(exp.Int64())
		}
	}
	return &key, nil
}

// Public returns the cached PublicKey, because the interface we're
// implementing doesn't want us to return errors, so, we'll force errors
// during startup.
//
// This has a downside of not being able to read the PublicKey if it changes
// during our session (womp), but maybe that's not a problem? Who can know.
// If that's a problem you hit, maybe we should do something smarter here.
func (s Store) Public() crypto.PublicKey {
	return s.PublicKey
}

// Sign implements crypto.Signer. This will have the HSM sign the hash given,
// ignoring the entropy source `rand` on chip, and return the signature blob.
func (s Store) Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	hash := opts.HashFunc()
	if len(digest) != hash.Size() {
		return nil, fmt.Errorf("pkcs11: Digest length doesn't match passed crypto algorithm")
	}

	privateKey, err := s.getObjectHandle(s.config.GetPrivateKeyTemplate())
	if err != nil {
		return nil, err
	}

	hashOID, ok := hashOIDs[hash]
	if !ok {
		return nil, fmt.Errorf("pkcs11: Unsupported algorithm")
	}
	digest = append(hashOID, digest...)

	if err := s.context.SignInit(
		*s.session,
		[]*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_RSA_PKCS, nil)},
		*privateKey,
	); err != nil {
		return nil, err
	}
	return s.context.Sign(*s.session, digest)
}

// Decrypt implements crypto.Decryptor. This will have the HSM Decrypt the
// encrypted data given, ignoring `rand`, and using on chip entropy sources.
// This will return the data in cleartext.
func (s Store) Decrypt(rand io.Reader, msg []byte, opts crypto.DecrypterOpts) ([]byte, error) {
	privateKey, err := s.getObjectHandle(s.config.GetPrivateKeyTemplate())
	if err != nil {
		return nil, err
	}

	if err := s.context.DecryptInit(
		*s.session,
		[]*pkcs11.Mechanism{pkcs11.NewMechanism(
			pkcs11.CKM_RSA_PKCS,
			nil,
		)},
		*privateKey,
	); err != nil {
		return nil, err
	}

	return s.context.Decrypt(*s.session, msg)
}

// TLSCertificate queries the underlying HSM Store for the x509 Certificate
// we're interested in, and returns a tls.Certificate containing both the
// public and private portions.
func (s Store) TLSCertificate() (*tls.Certificate, error) {
	cert, err := s.LoadCertificate()
	if err != nil {
		return nil, err
	}
	tlsCertificate := tls.Certificate{
		Certificate: [][]byte{cert.Raw},
		PrivateKey:  s,
		Leaf:        cert.Certificate,
	}
	return &tlsCertificate, nil
}

// vim: foldmethod=marker
