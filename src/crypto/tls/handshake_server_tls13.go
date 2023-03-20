// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tls

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"
)

// maxClientPSKIdentities is the number of client PSK identities the server will
// attempt to validate. It will ignore the rest not to let cheap ClientHello
// messages cause too much work in session ticket decryption attempts.
const maxClientPSKIdentities = 5

type serverHandshakeStateTLS13 struct {
	c               *Conn
	ctx             context.Context
	clientHello     *clientHelloMsg
	hello           *serverHelloMsg
	sentDummyCCS    bool
	usingPSK        bool
	suite           *cipherSuiteTLS13
	cert            *Certificate
	sigAlg          SignatureScheme
	earlySecret     []byte
	sharedKey       []byte
	handshakeSecret []byte
	masterSecret    []byte
	trafficSecret   []byte // client_application_traffic_secret_0
	transcript      hash.Hash
	clientFinished  []byte
	certReq         *certificateRequestMsgTLS13

	hsTimings CFEventTLS13ServerHandshakeTimingInfo
}

func (hs *serverHandshakeStateTLS13) echIsInner() bool {
	return len(hs.clientHello.ech) == 1 && hs.clientHello.ech[0] == echClientHelloInnerVariant
}

// processDelegatedCredentialFromClient unmarshals the DelegatedCredential
// offered by the client (if present) and validates it using the peer's
// certificate.
func (hs *serverHandshakeStateTLS13) processDelegatedCredentialFromClient(rawDC []byte, certVerifyMsg *certificateVerifyMsg) error {
	c := hs.c

	var dc *DelegatedCredential
	var err error
	if rawDC != nil {
		// Assert that the DC extension was indicated by the client.
		if !hs.certReq.supportDelegatedCredential {
			c.sendAlert(alertUnexpectedMessage)
			return errors.New("tls: got Delegated Credential extension without indication")
		}

		dc, err = UnmarshalDelegatedCredential(rawDC)
		if err != nil {
			c.sendAlert(alertDecodeError)
			return fmt.Errorf("tls: Delegated Credential: %s", err)
		}

		if !isSupportedSignatureAlgorithm(dc.cred.expCertVerfAlgo, supportedSignatureAlgorithmsDC) {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: Delegated Credential used with invalid signature algorithm")
		}
	}

	if dc != nil {
		if !dc.Validate(c.peerCertificates[0], true, c.config.time(), certVerifyMsg) {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: invalid Delegated Credential")
		}
	}

	c.verifiedDC = dc

	return nil
}

func (hs *serverHandshakeStateTLS13) handshake() error {
	c := hs.c

	if needFIPS() {
		return errors.New("tls: internal error: TLS 1.3 reached in FIPS mode")
	}

	// For an overview of the TLS 1.3 handshake, see RFC 8446, Section 2.
	if err := hs.processClientHello(); err != nil {
		return err
	}
	if err := hs.checkForResumption(); err != nil {
		return err
	}
	if err := hs.pickCertificate(); err != nil {
		return err
	}
	c.buffering = true
	if err := hs.sendServerParameters(); err != nil {
		return err
	}
	if err := hs.sendServerCertificate(); err != nil {
		return err
	}
	if err := hs.sendServerFinished(); err != nil {
		return err
	}
	// Note that at this point we could start sending application data without
	// waiting for the client's second flight, but the application might not
	// expect the lack of replay protection of the ClientHello parameters.
	if _, err := c.flush(); err != nil {
		return err
	}
	if err := hs.readClientCertificate(); err != nil {
		return err
	}
	if err := hs.readClientFinished(); err != nil {
		return err
	}

	c.handleCFEvent(hs.hsTimings)
	c.isHandshakeComplete.Store(true)

	return nil
}

func (hs *serverHandshakeStateTLS13) processClientHello() error {
	c := hs.c

	hs.hello = new(serverHelloMsg)

	// TLS 1.3 froze the ServerHello.legacy_version field, and uses
	// supported_versions instead. See RFC 8446, sections 4.1.3 and 4.2.1.
	hs.hello.vers = VersionTLS12
	hs.hello.supportedVersion = c.vers

	if len(hs.clientHello.supportedVersions) == 0 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: client used the legacy version field to negotiate TLS 1.3")
	}

	// Abort if the client is doing a fallback and landing lower than what we
	// support. See RFC 7507, which however does not specify the interaction
	// with supported_versions. The only difference is that with
	// supported_versions a client has a chance to attempt a [TLS 1.2, TLS 1.4]
	// handshake in case TLS 1.3 is broken but 1.2 is not. Alas, in that case,
	// it will have to drop the TLS_FALLBACK_SCSV protection if it falls back to
	// TLS 1.2, because a TLS 1.3 server would abort here. The situation before
	// supported_versions was not better because there was just no way to do a
	// TLS 1.4 handshake without risking the server selecting TLS 1.3.
	for _, id := range hs.clientHello.cipherSuites {
		if id == TLS_FALLBACK_SCSV {
			// Use c.vers instead of max(supported_versions) because an attacker
			// could defeat this by adding an arbitrary high version otherwise.
			if c.vers < c.config.maxSupportedVersion(roleServer) {
				c.sendAlert(alertInappropriateFallback)
				return errors.New("tls: client using inappropriate protocol fallback")
			}
			break
		}
	}

	if len(hs.clientHello.compressionMethods) != 1 ||
		hs.clientHello.compressionMethods[0] != compressionNone {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: TLS 1.3 client supports illegal compression methods")
	}

	hs.hello.random = make([]byte, 32)
	if _, err := io.ReadFull(c.config.rand(), hs.hello.random); err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	if len(hs.clientHello.secureRenegotiation) != 0 {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("tls: initial handshake had non-empty renegotiation extension")
	}

	if hs.clientHello.earlyData {
		// See RFC 8446, Section 4.2.10 for the complicated behavior required
		// here. The scenario is that a different server at our address offered
		// to accept early data in the past, which we can't handle. For now, all
		// 0-RTT enabled session tickets need to expire before a Go server can
		// replace a server or join a pool. That's the same requirement that
		// applies to mixing or replacing with any TLS 1.2 server.
		c.sendAlert(alertUnsupportedExtension)
		return errors.New("tls: client sent unexpected early data")
	}

	hs.hello.sessionId = hs.clientHello.sessionId
	hs.hello.compressionMethod = compressionNone

	preferenceList := defaultCipherSuitesTLS13
	if !hasAESGCMHardwareSupport || !aesgcmPreferred(hs.clientHello.cipherSuites) {
		preferenceList = defaultCipherSuitesTLS13NoAES
	}
	for _, suiteID := range preferenceList {
		hs.suite = mutualCipherSuiteTLS13(hs.clientHello.cipherSuites, suiteID)
		if hs.suite != nil {
			break
		}
	}
	if hs.suite == nil {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("tls: no cipher suite supported by both client and server")
	}
	c.cipherSuite = hs.suite.id
	hs.hello.cipherSuite = hs.suite.id
	hs.transcript = hs.suite.hash.New()

	// Resolve the server's preference for the ECDHE group.
	supportedCurves := c.config.curvePreferences()
	if testingTriggerHRR {
		// A HelloRetryRequest (HRR) is sent if the client does not offer a key
		// share for a curve supported by the server. To trigger this condition
		// intentionally, we compute the set of ECDHE groups supported by both
		// the client and server but for which the client did not offer a key
		// share.
		m := make(map[CurveID]bool)
		for _, serverGroup := range c.config.curvePreferences() {
			for _, clientGroup := range hs.clientHello.supportedCurves {
				if clientGroup == serverGroup {
					m[clientGroup] = true
				}
			}
		}
		for _, ks := range hs.clientHello.keyShares {
			delete(m, ks.group)
		}
		supportedCurves = nil
		for group := range m {
			supportedCurves = append(supportedCurves, group)
		}
		if len(supportedCurves) == 0 {
			// This occurs if the client offered a key share for each mutually
			// supported group.
			panic("failed to trigger HelloRetryRequest")
		}
	}

	// Pick the ECDHE group in server preference order, but give priority to
	// groups with a key share, to avoid a HelloRetryRequest round-trip.
	var selectedGroup CurveID
	var clientKeyShare *keyShare
GroupSelection:
	for _, preferredGroup := range supportedCurves {
		for _, ks := range hs.clientHello.keyShares {
			if ks.group == preferredGroup {
				selectedGroup = ks.group
				clientKeyShare = &ks
				break GroupSelection
			}
		}
		if selectedGroup != 0 {
			continue
		}
		for _, group := range hs.clientHello.supportedCurves {
			if group == preferredGroup {
				selectedGroup = group
				break
			}
		}
	}
	if selectedGroup == 0 {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("tls: no ECDHE curve supported by both client and server")
	}
	if clientKeyShare == nil {
		if err := hs.doHelloRetryRequest(selectedGroup); err != nil {
			return err
		}
		clientKeyShare = &hs.clientHello.keyShares[0]
	}

	if _, ok := curveForCurveID(selectedGroup); !ok {
		c.sendAlert(alertInternalError)
		return errors.New("tls: CurvePreferences includes unsupported curve")
	}
	key, err := generateECDHEKey(c.config.rand(), selectedGroup)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	hs.hello.serverShare = keyShare{group: selectedGroup, data: key.PublicKey().Bytes()}
	peerKey, err := key.Curve().NewPublicKey(clientKeyShare.data)
	if err != nil {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: invalid client key share")
	}
	hs.sharedKey, err = key.ECDH(peerKey)
	if err != nil {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: invalid client key share")
	}

	c.serverName = hs.clientHello.serverName

	hs.hsTimings.ProcessClientHello = hs.hsTimings.elapsedTime()

	return nil
}

func (hs *serverHandshakeStateTLS13) checkForResumption() error {
	c := hs.c

	if c.config.SessionTicketsDisabled || c.config.ECHEnabled {
		return nil
	}

	modeOK := false
	for _, mode := range hs.clientHello.pskModes {
		if mode == pskModeDHE {
			modeOK = true
			break
		}
	}
	if !modeOK {
		return nil
	}

	if len(hs.clientHello.pskIdentities) != len(hs.clientHello.pskBinders) {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: invalid or missing PSK binders")
	}
	if len(hs.clientHello.pskIdentities) == 0 {
		return nil
	}

	for i, identity := range hs.clientHello.pskIdentities {
		if i >= maxClientPSKIdentities {
			break
		}

		plaintext, _ := c.decryptTicket(identity.label)
		if plaintext == nil {
			continue
		}
		sessionState := new(sessionStateTLS13)
		if ok := sessionState.unmarshal(plaintext); !ok {
			continue
		}

		createdAt := time.Unix(int64(sessionState.createdAt), 0)
		if c.config.time().Sub(createdAt) > maxSessionTicketLifetime {
			continue
		}

		// We don't check the obfuscated ticket age because it's affected by
		// clock skew and it's only a freshness signal useful for shrinking the
		// window for replay attacks, which don't affect us as we don't do 0-RTT.

		pskSuite := cipherSuiteTLS13ByID(sessionState.cipherSuite)
		if pskSuite == nil || pskSuite.hash != hs.suite.hash {
			continue
		}

		// PSK connections don't re-establish client certificates, but carry
		// them over in the session ticket. Ensure the presence of client certs
		// in the ticket is consistent with the configured requirements.
		sessionHasClientCerts := len(sessionState.certificate.Certificate) != 0
		needClientCerts := requiresClientCert(c.config.ClientAuth)
		if needClientCerts && !sessionHasClientCerts {
			continue
		}
		if sessionHasClientCerts && c.config.ClientAuth == NoClientCert {
			continue
		}

		psk := hs.suite.expandLabel(sessionState.resumptionSecret, "resumption",
			nil, hs.suite.hash.Size())
		hs.earlySecret = hs.suite.extract(psk, nil)
		binderKey := hs.suite.deriveSecret(hs.earlySecret, resumptionBinderLabel, nil)
		// Clone the transcript in case a HelloRetryRequest was recorded.
		transcript := cloneHash(hs.transcript, hs.suite.hash)
		if transcript == nil {
			c.sendAlert(alertInternalError)
			return errors.New("tls: internal error: failed to clone hash")
		}
		clientHelloBytes, err := hs.clientHello.marshalWithoutBinders()
		if err != nil {
			c.sendAlert(alertInternalError)
			return err
		}
		transcript.Write(clientHelloBytes)
		pskBinder := hs.suite.finishedHash(binderKey, transcript)
		if !hmac.Equal(hs.clientHello.pskBinders[i], pskBinder) {
			c.sendAlert(alertDecryptError)
			return errors.New("tls: invalid PSK binder")
		}

		c.didResume = true
		if err := c.processCertsFromClient(sessionState.certificate); err != nil {
			return err
		}

		hs.hello.selectedIdentityPresent = true
		hs.hello.selectedIdentity = uint16(i)
		hs.usingPSK = true
		return nil
	}

	return nil
}

// cloneHash uses the encoding.BinaryMarshaler and encoding.BinaryUnmarshaler
// interfaces implemented by standard library hashes to clone the state of in
// to a new instance of h. It returns nil if the operation fails.
func cloneHash(in hash.Hash, h crypto.Hash) hash.Hash {
	// Recreate the interface to avoid importing encoding.
	type binaryMarshaler interface {
		MarshalBinary() (data []byte, err error)
		UnmarshalBinary(data []byte) error
	}
	marshaler, ok := in.(binaryMarshaler)
	if !ok {
		return nil
	}
	state, err := marshaler.MarshalBinary()
	if err != nil {
		return nil
	}
	out := h.New()
	unmarshaler, ok := out.(binaryMarshaler)
	if !ok {
		return nil
	}
	if err := unmarshaler.UnmarshalBinary(state); err != nil {
		return nil
	}
	return out
}

// getDelegatedCredential will return a Delegated Credential pair (a Delegated
// Credential and its private key) for the given ClientHelloInfo, defaulting to
// the first element of cert.DelegatedCredentialPair.
// The returned Delegated Credential could be invalid for usage in the handshake.
// Returns an error if there are no delegated credentials or if the one found
// cannot be used for the current connection.
func getDelegatedCredential(clientHello *ClientHelloInfo, cert *Certificate) (*DelegatedCredentialPair, error) {
	if len(cert.DelegatedCredentials) == 0 {
		return nil, errors.New("no Delegated Credential found")
	}

	for _, dcPair := range cert.DelegatedCredentials {
		// The client must have sent the signature_algorithms in the DC extension: ensure it supports
		// schemes we can use with this delegated credential.
		if len(clientHello.SignatureSchemesDC) > 0 {
			if _, err := selectSignatureSchemeDC(VersionTLS13, dcPair.DC, clientHello.SignatureSchemes, clientHello.SignatureSchemesDC); err == nil {
				return &dcPair, nil
			}
		}
	}

	// No delegated credential can be returned.
	return nil, errors.New("no valid Delegated Credential found")
}

func (hs *serverHandshakeStateTLS13) pickCertificate() error {
	c := hs.c

	// Only one of PSK and certificates are used at a time.
	if hs.usingPSK {
		return nil
	}

	// signature_algorithms is required in TLS 1.3. See RFC 8446, Section 4.2.3.
	if len(hs.clientHello.supportedSignatureAlgorithms) == 0 {
		return c.sendAlert(alertMissingExtension)
	}

	certificate, err := c.config.getCertificate(clientHelloInfo(hs.ctx, c, hs.clientHello))
	if err != nil {
		if err == errNoCertificates {
			c.sendAlert(alertUnrecognizedName)
		} else {
			c.sendAlert(alertInternalError)
		}
		return err
	}

	hs.sigAlg, err = selectSignatureScheme(c.vers, certificate, hs.clientHello.supportedSignatureAlgorithms)
	if err != nil {
		// getCertificate returned a certificate that is unsupported or
		// incompatible with the client's signature algorithms.
		c.sendAlert(alertHandshakeFailure)
		return err
	}

	hs.cert = certificate

	if hs.clientHello.delegatedCredentialSupported && len(hs.clientHello.supportedSignatureAlgorithmsDC) > 0 {
		// getDelegatedCredential selects a delegated credential that the client has advertised support for, if possible.
		delegatedCredentialPair, err := getDelegatedCredential(clientHelloInfo(hs.ctx, c, hs.clientHello), hs.cert)
		if err != nil {
			// a Delegated Credential was not found. Fallback to the certificate.
			return nil
		}
		if delegatedCredentialPair.DC != nil && delegatedCredentialPair.PrivateKey != nil {
			// Even if the Delegated Credential has already been marshalled, be sure it is the correct one.
			delegatedCredentialPair.DC.raw, err = delegatedCredentialPair.DC.Marshal()
			if err != nil {
				// invalid Delegated Credential. Fallback to the certificate.
				return nil
			}
			hs.sigAlg = delegatedCredentialPair.DC.cred.expCertVerfAlgo

			hs.cert.PrivateKey = delegatedCredentialPair.PrivateKey
			hs.cert.DelegatedCredential = delegatedCredentialPair.DC.raw
		}
	}
	return nil
}

// sendDummyChangeCipherSpec sends a ChangeCipherSpec record for compatibility
// with middleboxes that didn't implement TLS correctly. See RFC 8446, Appendix D.4.
func (hs *serverHandshakeStateTLS13) sendDummyChangeCipherSpec() error {
	if hs.sentDummyCCS {
		return nil
	}
	hs.sentDummyCCS = true

	return hs.c.writeChangeCipherRecord()
}

func (hs *serverHandshakeStateTLS13) doHelloRetryRequest(selectedGroup CurveID) error {
	c := hs.c

	// The first ClientHello gets double-hashed into the transcript upon a
	// HelloRetryRequest. See RFC 8446, Section 4.4.1.
	if err := transcriptMsg(hs.clientHello, hs.transcript); err != nil {
		return err
	}
	chHash := hs.transcript.Sum(nil)
	hs.transcript.Reset()
	hs.transcript.Write([]byte{typeMessageHash, 0, 0, uint8(len(chHash))})
	hs.transcript.Write(chHash)

	helloRetryRequest := &serverHelloMsg{
		vers:              hs.hello.vers,
		random:            helloRetryRequestRandom,
		sessionId:         hs.hello.sessionId,
		cipherSuite:       hs.hello.cipherSuite,
		compressionMethod: hs.hello.compressionMethod,
		supportedVersion:  hs.hello.supportedVersion,
		selectedGroup:     selectedGroup,
	}

	// Decide whether to send "encrypted_client_hello" extension.
	if hs.echIsInner() {
		// Confirm ECH acceptance if this is the inner handshake.
		echAcceptConfHRRTranscript := cloneHash(hs.transcript, hs.suite.hash)
		if echAcceptConfHRRTranscript == nil {
			c.sendAlert(alertInternalError)
			return errors.New("tls: internal error: failed to clone hash")
		}

		helloRetryRequest.ech = zeros[:8]
		echAcceptConfHRR, err := helloRetryRequest.marshal()
		if err != nil {
			return fmt.Errorf("tls: ech: HRR.marshal(): %w", err)
		}
		echAcceptConfHRRTranscript.Write(echAcceptConfHRR)
		echAcceptConfHRRSignal := hs.suite.expandLabel(
			hs.suite.extract(hs.clientHello.random, nil),
			echAcceptConfHRRLabel,
			echAcceptConfHRRTranscript.Sum(nil),
			8)

		helloRetryRequest.ech = echAcceptConfHRRSignal
		helloRetryRequest.raw = nil
	} else if c.ech.status == echStatusFailure {
		// draft-ietf-tls-esni-13, Section 7.1:
		//
		// If sending a HelloRetryRequest, the server MAY include an
		// "encrypted_client_hello" extension with a payload of 8 random bytes;
		// see Section 10.9.4 for details.
		helloRetryRequest.ech = make([]byte, 8)
		if _, err := io.ReadFull(c.config.rand(), helloRetryRequest.ech); err != nil {
			c.sendAlert(alertInternalError)
			return fmt.Errorf("tls: internal error: rng failure: %s", err)
		}
	}

	if _, err := hs.c.writeHandshakeRecord(helloRetryRequest, hs.transcript); err != nil {
		return err
	}

	if err := hs.sendDummyChangeCipherSpec(); err != nil {
		return err
	}

	// clientHelloMsg is not included in the transcript.
	msg, err := c.readHandshake(nil)
	if err != nil {
		return err
	}

	clientHello, ok := msg.(*clientHelloMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(clientHello, msg)
	}

	clientHello, err = c.echAcceptOrReject(clientHello, true) // afterHRR == true
	if err != nil {
		return fmt.Errorf("tls: %s", err) // Alert sent
	}

	if len(clientHello.keyShares) != 1 || clientHello.keyShares[0].group != selectedGroup {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: client sent invalid key share in second ClientHello")
	}

	if clientHello.earlyData {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: client indicated early data in second ClientHello")
	}

	if illegalClientHelloChange(clientHello, hs.clientHello) {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: client illegally modified second ClientHello")
	}

	hs.clientHello = clientHello
	return nil
}

// illegalClientHelloChange reports whether the two ClientHello messages are
// different, with the exception of the changes allowed before and after a
// HelloRetryRequest. See RFC 8446, Section 4.1.2.
func illegalClientHelloChange(ch, ch1 *clientHelloMsg) bool {
	if len(ch.supportedVersions) != len(ch1.supportedVersions) ||
		len(ch.cipherSuites) != len(ch1.cipherSuites) ||
		len(ch.supportedCurves) != len(ch1.supportedCurves) ||
		len(ch.supportedSignatureAlgorithms) != len(ch1.supportedSignatureAlgorithms) ||
		len(ch.supportedSignatureAlgorithmsCert) != len(ch1.supportedSignatureAlgorithmsCert) ||
		len(ch.supportedSignatureAlgorithmsDC) != len(ch1.supportedSignatureAlgorithmsDC) ||
		len(ch.alpnProtocols) != len(ch1.alpnProtocols) {
		return true
	}
	for i := range ch.supportedVersions {
		if ch.supportedVersions[i] != ch1.supportedVersions[i] {
			return true
		}
	}
	for i := range ch.cipherSuites {
		if ch.cipherSuites[i] != ch1.cipherSuites[i] {
			return true
		}
	}
	for i := range ch.supportedCurves {
		if ch.supportedCurves[i] != ch1.supportedCurves[i] {
			return true
		}
	}
	for i := range ch.supportedSignatureAlgorithms {
		if ch.supportedSignatureAlgorithms[i] != ch1.supportedSignatureAlgorithms[i] {
			return true
		}
	}
	for i := range ch.supportedSignatureAlgorithmsCert {
		if ch.supportedSignatureAlgorithmsCert[i] != ch1.supportedSignatureAlgorithmsCert[i] {
			return true
		}
	}
	for i := range ch.supportedSignatureAlgorithmsDC {
		if ch.supportedSignatureAlgorithmsDC[i] != ch1.supportedSignatureAlgorithmsDC[i] {
			return true
		}
	}
	for i := range ch.alpnProtocols {
		if ch.alpnProtocols[i] != ch1.alpnProtocols[i] {
			return true
		}
	}
	return ch.vers != ch1.vers ||
		!bytes.Equal(ch.random, ch1.random) ||
		!bytes.Equal(ch.sessionId, ch1.sessionId) ||
		!bytes.Equal(ch.compressionMethods, ch1.compressionMethods) ||
		ch.serverName != ch1.serverName ||
		ch.ocspStapling != ch1.ocspStapling ||
		!bytes.Equal(ch.supportedPoints, ch1.supportedPoints) ||
		ch.ticketSupported != ch1.ticketSupported ||
		!bytes.Equal(ch.sessionTicket, ch1.sessionTicket) ||
		ch.secureRenegotiationSupported != ch1.secureRenegotiationSupported ||
		!bytes.Equal(ch.secureRenegotiation, ch1.secureRenegotiation) ||
		ch.delegatedCredentialSupported != ch1.delegatedCredentialSupported ||
		ch.scts != ch1.scts ||
		!bytes.Equal(ch.cookie, ch1.cookie) ||
		!bytes.Equal(ch.pskModes, ch1.pskModes)
}

func (hs *serverHandshakeStateTLS13) sendServerParameters() error {
	c := hs.c

	// Confirm ECH acceptance.
	if hs.echIsInner() {
		// Clear the last 8 bytes of the ServerHello.random in preparation for
		// computing the confirmation hint.
		copy(hs.hello.random[24:], zeros[:8])

		// Set the last 8 bytes of ServerHello.random to a string derived from
		// the inner handshake.
		echAcceptConfTranscript := cloneHash(hs.transcript, hs.suite.hash)
		if echAcceptConfTranscript == nil {
			c.sendAlert(alertInternalError)
			return errors.New("tls: internal error: failed to clone hash")
		}
		chMarshalled, err := hs.clientHello.marshal()
		if err != nil {
			return fmt.Errorf("tls: ech: clientHello.marshal(): %w", err)
		}
		echAcceptConfTranscript.Write(chMarshalled)
		hMarshalled, err := hs.hello.marshal()
		if err != nil {
			return fmt.Errorf("tls: ech: hello.marshal(): %w", err)
		}
		echAcceptConfTranscript.Write(hMarshalled)

		echAcceptConf := hs.suite.expandLabel(
			hs.suite.extract(hs.clientHello.random, nil),
			echAcceptConfLabel,
			echAcceptConfTranscript.Sum(nil),
			8)

		copy(hs.hello.random[24:], echAcceptConf)
		hs.hello.raw = nil
	}

	if err := transcriptMsg(hs.clientHello, hs.transcript); err != nil {
		return err
	}
	if _, err := hs.c.writeHandshakeRecord(hs.hello, hs.transcript); err != nil {
		return err
	}

	hs.hsTimings.WriteServerHello = hs.hsTimings.elapsedTime()

	if err := hs.sendDummyChangeCipherSpec(); err != nil {
		return err
	}

	earlySecret := hs.earlySecret
	if earlySecret == nil {
		earlySecret = hs.suite.extract(nil, nil)
	}
	hs.handshakeSecret = hs.suite.extract(hs.sharedKey,
		hs.suite.deriveSecret(earlySecret, "derived", nil))

	clientSecret := hs.suite.deriveSecret(hs.handshakeSecret,
		clientHandshakeTrafficLabel, hs.transcript)
	c.in.setTrafficSecret(hs.suite, clientSecret)
	serverSecret := hs.suite.deriveSecret(hs.handshakeSecret,
		serverHandshakeTrafficLabel, hs.transcript)
	c.out.setTrafficSecret(hs.suite, serverSecret)

	err := c.config.writeKeyLog(keyLogLabelClientHandshake, hs.clientHello.random, clientSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	err = c.config.writeKeyLog(keyLogLabelServerHandshake, hs.clientHello.random, serverSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	encryptedExtensions := new(encryptedExtensionsMsg)

	selectedProto, err := negotiateALPN(c.config.NextProtos, hs.clientHello.alpnProtocols)
	if err != nil {
		c.sendAlert(alertNoApplicationProtocol)
		return err
	}
	encryptedExtensions.alpnProtocol = selectedProto
	c.clientProtocol = selectedProto

	if c.ech.status == echStatusFailure && len(c.ech.retryConfigs) > 0 {
		encryptedExtensions.ech = c.ech.retryConfigs
	}

	if _, err := hs.c.writeHandshakeRecord(encryptedExtensions, hs.transcript); err != nil {
		return err
	}

	hs.hsTimings.WriteEncryptedExtensions = hs.hsTimings.elapsedTime()

	return nil
}

func (hs *serverHandshakeStateTLS13) requestClientCert() bool {
	return hs.c.config.ClientAuth >= RequestClientCert && !hs.usingPSK
}

func (hs *serverHandshakeStateTLS13) sendServerCertificate() error {
	c := hs.c

	// Only one of PSK and certificates are used at a time.
	if hs.usingPSK {
		return nil
	}

	if hs.requestClientCert() {
		// Request a client certificate
		certReq := new(certificateRequestMsgTLS13)
		certReq.ocspStapling = true
		certReq.scts = true
		certReq.supportedSignatureAlgorithms = c.config.supportedSignatureAlgorithms()
		certReq.supportDelegatedCredential = c.config.SupportDelegatedCredential
		certReq.supportedSignatureAlgorithmsDC = supportedSignatureAlgorithmsDC
		if c.config.ClientCAs != nil {
			certReq.certificateAuthorities = c.config.ClientCAs.Subjects()
		}

		hs.certReq = certReq
		if _, err := hs.c.writeHandshakeRecord(certReq, hs.transcript); err != nil {
			return err
		}
	}

	certMsg := new(certificateMsgTLS13)

	certMsg.certificate = *hs.cert
	certMsg.scts = hs.clientHello.scts && len(hs.cert.SignedCertificateTimestamps) > 0
	certMsg.ocspStapling = hs.clientHello.ocspStapling && len(hs.cert.OCSPStaple) > 0
	certMsg.delegatedCredential = hs.clientHello.delegatedCredentialSupported && len(hs.cert.DelegatedCredential) > 0

	if _, err := hs.c.writeHandshakeRecord(certMsg, hs.transcript); err != nil {
		return err
	}

	hs.hsTimings.WriteCertificate = hs.hsTimings.elapsedTime()

	certVerifyMsg := new(certificateVerifyMsg)
	certVerifyMsg.hasSignatureAlgorithm = true
	certVerifyMsg.signatureAlgorithm = hs.sigAlg
	sigType, sigHash, err := typeAndHashFromSignatureScheme(certVerifyMsg.signatureAlgorithm)
	if err != nil {
		return c.sendAlert(alertInternalError)
	}

	signed := signedMessage(sigHash, serverSignatureContext, hs.transcript)
	signOpts := crypto.SignerOpts(sigHash)
	if sigType == signatureRSAPSS {
		signOpts = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: sigHash}
	}
	sig, err := hs.cert.PrivateKey.(crypto.Signer).Sign(c.config.rand(), signed, signOpts)
	if err != nil {
		public := hs.cert.PrivateKey.(crypto.Signer).Public()
		if rsaKey, ok := public.(*rsa.PublicKey); ok && sigType == signatureRSAPSS &&
			rsaKey.N.BitLen()/8 < sigHash.Size()*2+2 { // key too small for RSA-PSS
			c.sendAlert(alertHandshakeFailure)
		} else {
			c.sendAlert(alertInternalError)
		}
		return errors.New("tls: failed to sign handshake: " + err.Error())
	}
	certVerifyMsg.signature = sig

	if _, err := hs.c.writeHandshakeRecord(certVerifyMsg, hs.transcript); err != nil {
		return err
	}

	hs.hsTimings.WriteCertificateVerify = hs.hsTimings.elapsedTime()

	return nil
}

func (hs *serverHandshakeStateTLS13) sendServerFinished() error {
	c := hs.c

	finished := &finishedMsg{
		verifyData: hs.suite.finishedHash(c.out.trafficSecret, hs.transcript),
	}

	if _, err := hs.c.writeHandshakeRecord(finished, hs.transcript); err != nil {
		return err
	}

	hs.hsTimings.WriteServerFinished = hs.hsTimings.elapsedTime()

	// Derive secrets that take context through the server Finished.

	hs.masterSecret = hs.suite.extract(nil,
		hs.suite.deriveSecret(hs.handshakeSecret, "derived", nil))

	hs.trafficSecret = hs.suite.deriveSecret(hs.masterSecret,
		clientApplicationTrafficLabel, hs.transcript)
	serverSecret := hs.suite.deriveSecret(hs.masterSecret,
		serverApplicationTrafficLabel, hs.transcript)
	c.out.setTrafficSecret(hs.suite, serverSecret)

	err := c.config.writeKeyLog(keyLogLabelClientTraffic, hs.clientHello.random, hs.trafficSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	err = c.config.writeKeyLog(keyLogLabelServerTraffic, hs.clientHello.random, serverSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	c.ekm = hs.suite.exportKeyingMaterial(hs.masterSecret, hs.transcript)

	// If we did not request client certificates, at this point we can
	// precompute the client finished and roll the transcript forward to send
	// session tickets in our first flight.
	if !hs.requestClientCert() {
		if err := hs.sendSessionTickets(); err != nil {
			return err
		}
	}

	return nil
}

func (hs *serverHandshakeStateTLS13) shouldSendSessionTickets() bool {
	if hs.c.config.SessionTicketsDisabled || hs.c.config.ECHEnabled {
		return false
	}

	// Don't send tickets the client wouldn't use. See RFC 8446, Section 4.2.9.
	for _, pskMode := range hs.clientHello.pskModes {
		if pskMode == pskModeDHE {
			return true
		}
	}
	return false
}

func (hs *serverHandshakeStateTLS13) sendSessionTickets() error {
	c := hs.c

	hs.clientFinished = hs.suite.finishedHash(c.in.trafficSecret, hs.transcript)
	finishedMsg := &finishedMsg{
		verifyData: hs.clientFinished,
	}
	if err := transcriptMsg(finishedMsg, hs.transcript); err != nil {
		return err
	}

	if !hs.shouldSendSessionTickets() {
		return nil
	}

	resumptionSecret := hs.suite.deriveSecret(hs.masterSecret,
		resumptionLabel, hs.transcript)

	m := new(newSessionTicketMsgTLS13)

	var certsFromClient [][]byte
	for _, cert := range c.peerCertificates {
		certsFromClient = append(certsFromClient, cert.Raw)
	}
	state := sessionStateTLS13{
		cipherSuite:      hs.suite.id,
		createdAt:        uint64(c.config.time().Unix()),
		resumptionSecret: resumptionSecret,
		certificate: Certificate{
			Certificate:                 certsFromClient,
			OCSPStaple:                  c.ocspResponse,
			SignedCertificateTimestamps: c.scts,
		},
	}
	stateBytes, err := state.marshal()
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	m.label, err = c.encryptTicket(stateBytes)
	if err != nil {
		return err
	}
	m.lifetime = uint32(maxSessionTicketLifetime / time.Second)

	// ticket_age_add is a random 32-bit value. See RFC 8446, section 4.6.1
	// The value is not stored anywhere; we never need to check the ticket age
	// because 0-RTT is not supported.
	ageAdd := make([]byte, 4)
	_, err = hs.c.config.rand().Read(ageAdd)
	if err != nil {
		return err
	}
	m.ageAdd = binary.LittleEndian.Uint32(ageAdd)

	// ticket_nonce, which must be unique per connection, is always left at
	// zero because we only ever send one ticket per connection.

	if _, err := c.writeHandshakeRecord(m, nil); err != nil {
		return err
	}

	return nil
}

func (hs *serverHandshakeStateTLS13) readClientCertificate() error {
	c := hs.c

	if !hs.requestClientCert() {
		// Make sure the connection is still being verified whether or not
		// the server requested a client certificate.
		if c.config.VerifyConnection != nil {
			if err := c.config.VerifyConnection(c.connectionStateLocked()); err != nil {
				c.sendAlert(alertBadCertificate)
				return err
			}
		}
		return nil
	}

	// If we requested a client certificate, then the client must send a
	// certificate message. If it's empty, no CertificateVerify is sent.

	msg, err := c.readHandshake(hs.transcript)
	if err != nil {
		return err
	}

	certMsg, ok := msg.(*certificateMsgTLS13)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(certMsg, msg)
	}

	if err := c.processCertsFromClient(certMsg.certificate); err != nil {
		return err
	}

	if c.config.VerifyConnection != nil {
		if err := c.config.VerifyConnection(c.connectionStateLocked()); err != nil {
			c.sendAlert(alertBadCertificate)
			return err
		}
	}

	hs.hsTimings.ReadCertificate = hs.hsTimings.elapsedTime()

	if len(certMsg.certificate.Certificate) != 0 {
		// certificateVerifyMsg is included in the transcript, but not until
		// after we verify the handshake signature, since the state before
		// this message was sent is used.
		msg, err = c.readHandshake(nil)
		if err != nil {
			return err
		}

		certVerify, ok := msg.(*certificateVerifyMsg)
		if !ok {
			c.sendAlert(alertUnexpectedMessage)
			return unexpectedMessageError(certVerify, msg)
		}

		// See RFC 8446, Section 4.4.3.
		if !isSupportedSignatureAlgorithm(certVerify.signatureAlgorithm, c.config.supportedSignatureAlgorithms()) {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: client certificate used with invalid signature algorithm")
		}
		sigType, sigHash, err := typeAndHashFromSignatureScheme(certVerify.signatureAlgorithm)
		if err != nil {
			return c.sendAlert(alertInternalError)
		}
		if sigType == signaturePKCS1v15 || sigHash == crypto.SHA1 {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: client certificate used with invalid signature algorithm")
		}

		if certMsg.delegatedCredential {
			if err := hs.processDelegatedCredentialFromClient(certMsg.certificate.DelegatedCredential, certVerify); err != nil {
				return err
			}
		}

		pk := c.peerCertificates[0].PublicKey
		if c.verifiedDC != nil {
			pk = c.verifiedDC.cred.publicKey
		}

		signed := signedMessage(sigHash, clientSignatureContext, hs.transcript)
		if err := verifyHandshakeSignature(sigType, pk, sigHash, signed, certVerify.signature); err != nil {
			c.sendAlert(alertDecryptError)
			return errors.New("tls: invalid signature by the client certificate: " + err.Error())
		}

		if err := transcriptMsg(certVerify, hs.transcript); err != nil {
			return err
		}
	}

	hs.hsTimings.ReadCertificateVerify = hs.hsTimings.elapsedTime()

	// If we waited until the client certificates to send session tickets, we
	// are ready to do it now.
	if err := hs.sendSessionTickets(); err != nil {
		return err
	}

	return nil
}

func (hs *serverHandshakeStateTLS13) readClientFinished() error {
	c := hs.c

	// finishedMsg is not included in the transcript.
	msg, err := c.readHandshake(nil)
	if err != nil {
		return err
	}

	finished, ok := msg.(*finishedMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(finished, msg)
	}

	if !hmac.Equal(hs.clientFinished, finished.verifyData) {
		c.sendAlert(alertDecryptError)
		return errors.New("tls: invalid client finished hash")
	}

	hs.hsTimings.ReadClientFinished = hs.hsTimings.elapsedTime()

	c.in.setTrafficSecret(hs.suite, hs.trafficSecret)

	return nil
}
