package tpm

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/asn1"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"

	"github.com/google/go-tpm/tpm2"
)

// AttestationQuote holds a TPM2 quote. It carries no key: the verifier
// checks it against an AK public key that it trusts (see VerifyQuote).
type AttestationQuote struct {
	// QuoteData is the TPMS_ATTEST structure that the AK signed.
	QuoteData []byte `json:"quoteData"`
	// Signature is the ASN.1 DER ECDSA signature over SHA-256(QuoteData).
	Signature []byte `json:"signature"`
	// PCRValues are the SHA-256 PCR values of the quoted selection, read
	// after the quote. They are not signed; VerifyQuote checks that they
	// hash to the signed PCR digest.
	PCRValues map[int][]byte `json:"pcrValues"`
	// Nonce is the qualifying data that the device used.
	Nonce []byte `json:"nonce"`
}

// MarshalQuote serializes a quote to JSON.
func MarshalQuote(quote *AttestationQuote) ([]byte, error) {
	data, err := json.Marshal(quote)
	if err != nil {
		return nil, fmt.Errorf("marshaling quote: %w", err)
	}
	return data, nil
}

// Errors of VerifyQuote.
var (
	ErrAKNotRestricted  = errors.New("AK is not a restricted fixedTPM signing key")
	ErrEKNotTCG         = errors.New("EK is not a restricted fixedTPM decryption key with the TCG EK policy")
	ErrQuoteSignature   = errors.New("quote signature does not verify with the trusted AK")
	ErrQuoteNonce       = errors.New("quote nonce does not match the expected nonce")
	ErrQuotePCRMismatch = errors.New("PCR values do not match the signed PCR digest")
)

// parseAK returns the ECDSA key of an AK public area and checks that it is
// a restricted, fixedTPM, TPM-generated ECDSA P-256 SHA-256 signing key.
func parseAK(akPublic []byte) (*ecdsa.PublicKey, *tpm2.TPMTPublic, error) {
	pub, err := tpm2.Unmarshal[tpm2.TPMTPublic](akPublic)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing AK public: %w", err)
	}
	if pub.Type != tpm2.TPMAlgECC || !restrictedSigningKey(pub.ObjectAttributes) {
		return nil, nil, ErrAKNotRestricted
	}
	params, err := pub.Parameters.ECCDetail()
	if err != nil {
		return nil, nil, fmt.Errorf("AK parameters: %w", err)
	}
	if params.CurveID != tpm2.TPMECCNistP256 || params.Scheme.Scheme != tpm2.TPMAlgECDSA {
		return nil, nil, fmt.Errorf("%w: want ECDSA P-256", ErrAKNotRestricted)
	}
	if s, err := params.Scheme.Details.ECDSA(); err != nil || s.HashAlg != tpm2.TPMAlgSHA256 {
		return nil, nil, fmt.Errorf("%w: want the SHA-256 ECDSA scheme", ErrAKNotRestricted)
	}
	point, err := pub.Unique.ECC()
	if err != nil {
		return nil, nil, fmt.Errorf("AK public point: %w", err)
	}
	const coordLen = 32 // P-256
	if len(point.X.Buffer) > coordLen || len(point.Y.Buffer) > coordLen {
		return nil, nil, fmt.Errorf("AK public point: coordinates longer than %d bytes", coordLen)
	}
	// The uncompressed point 0x04 || X || Y, the coordinates left-padded.
	// The parser rejects a point that is not on the curve.
	raw := make([]byte, 1+2*coordLen)
	raw[0] = 4
	copy(raw[1+coordLen-len(point.X.Buffer):1+coordLen], point.X.Buffer)
	copy(raw[1+2*coordLen-len(point.Y.Buffer):], point.Y.Buffer)
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		return nil, nil, fmt.Errorf("AK public point: %w", err)
	}
	return key, pub, nil
}

// restrictedSigningKey reports whether a key signs only TPM-generated data
// (restricted, sign only) and never left the TPM it was generated in.
func restrictedSigningKey(a tpm2.TPMAObject) bool {
	return a.Restricted && a.SignEncrypt && !a.Decrypt && a.FixedTPM && a.FixedParent && a.SensitiveDataOrigin
}

// checkEK checks that an EK public area has the attributes and the auth
// policy (PolicySecret(TPM_RH_ENDORSEMENT)) of the TCG EK templates: a
// restricted decryption key that never left the TPM it was generated in.
func checkEK(ek *tpm2.TPMTPublic) error {
	a := ek.ObjectAttributes
	if !a.Restricted || !a.Decrypt || a.SignEncrypt || !a.FixedTPM || !a.FixedParent || !a.SensitiveDataOrigin ||
		!bytes.Equal(ek.AuthPolicy.Buffer, ekTCGPolicy) {
		return ErrEKNotTCG
	}
	return nil
}

// ekTCGPolicy is the auth policy digest of the TCG EK templates (low range,
// SHA-256): PolicySecret(TPM_RH_ENDORSEMENT).
var ekTCGPolicy = tpm2.RSAEKTemplate.AuthPolicy.Buffer

// MakeCredential is the verifier side of credential activation. It checks
// that akPublic is a restricted signing key and that ekPublic has the TCG
// EK attributes and policy, and encrypts secret to the EK so that only the
// TPM holding this EK and this AK can recover it with ActivateCredential.
// The verifier must trust ekPublic first (EK certificate check): without
// it, a successful activation proves only that the same TPM holds the EK
// and the AK, not that it is a genuine TPM. Send credentialBlob and
// encryptedSecret to the device.
func MakeCredential(ekPublic, akPublic, secret []byte) (credentialBlob, encryptedSecret []byte, err error) {
	_, ak, err := parseAK(akPublic)
	if err != nil {
		return nil, nil, err
	}
	name, err := tpm2.ObjectName(ak)
	if err != nil {
		return nil, nil, fmt.Errorf("AK name: %w", err)
	}
	ek, err := tpm2.Unmarshal[tpm2.TPMTPublic](ekPublic)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing EK public: %w", err)
	}
	if err := checkEK(ek); err != nil {
		return nil, nil, err
	}
	ekKey, err := tpm2.ImportEncapsulationKey(ek)
	if err != nil {
		return nil, nil, fmt.Errorf("EK: %w", err)
	}
	blob, enc, err := tpm2.CreateCredential(rand.Reader, ekKey, name.Buffer, secret)
	if err != nil {
		return nil, nil, fmt.Errorf("creating credential: %w", err)
	}
	return blob, enc, nil
}

// VerifyQuote checks a quote against an AK public area that the caller
// trusts (from credential activation, never from the quote) and the nonce
// that the caller sent:
//   - the AK is a restricted signing key and its signature over QuoteData
//     verifies;
//   - QuoteData is a quote whose qualifying data is nonce;
//   - the quote covers exactly the SHA-256 PCRs in quote.PCRValues and their
//     values hash to the signed PCR digest.
//
// Then quote.PCRValues are authentic; compare them with the expected values
// (VerifyQuoteAgainstPolicy, ReplayMeasurementLog).
func VerifyQuote(akPublic []byte, quote *AttestationQuote, nonce []byte) error {
	if quote == nil {
		return errors.New("nil quote")
	}
	if len(nonce) == 0 {
		return errors.New("empty expected nonce")
	}
	key, _, err := parseAK(akPublic)
	if err != nil {
		return err
	}
	// A restricted key signs only TPM-generated data (TPM_GENERATED_VALUE),
	// so a valid signature proves that the TPM made QuoteData.
	digest := sha256.Sum256(quote.QuoteData)
	if !ecdsa.VerifyASN1(key, digest[:], quote.Signature) {
		return ErrQuoteSignature
	}
	attest, err := tpm2.Unmarshal[tpm2.TPMSAttest](quote.QuoteData)
	if err != nil {
		return fmt.Errorf("parsing quote: %w", err)
	}
	// Defense in depth: the restricted AK already signs only data that
	// starts with TPM_GENERATED_VALUE.
	if attest.Magic != tpm2.TPMGeneratedValue {
		return errors.New("quote data is not TPM-generated")
	}
	if subtle.ConstantTimeCompare(attest.ExtraData.Buffer, nonce) != 1 {
		return ErrQuoteNonce
	}
	info, err := attest.Attested.Quote()
	if err != nil {
		return fmt.Errorf("quote info: %w", err)
	}
	indices, err := sha256Selection(info.PCRSelect)
	if err != nil {
		return err
	}
	want := slices.Sorted(maps.Keys(quote.PCRValues))
	if !slices.Equal(indices, want) {
		return fmt.Errorf("%w: the quote covers PCRs %v, the values name %v", ErrQuotePCRMismatch, indices, want)
	}
	h := sha256.New()
	for _, idx := range indices {
		h.Write(quote.PCRValues[idx])
	}
	if !bytes.Equal(h.Sum(nil), info.PCRDigest.Buffer) {
		return ErrQuotePCRMismatch
	}
	return nil
}

// sha256Selection returns the PCR indices of a selection that has only the
// SHA-256 bank, in ascending order (the order of the TPM PCR digest).
func sha256Selection(sel tpm2.TPMLPCRSelection) ([]int, error) {
	if len(sel.PCRSelections) != 1 || sel.PCRSelections[0].Hash != tpm2.TPMAlgSHA256 {
		return nil, fmt.Errorf("%w: the quote must select the SHA-256 bank only", ErrQuotePCRMismatch)
	}
	var indices []int
	for i, b := range sel.PCRSelections[0].PCRSelect {
		for bit := range 8 {
			if b&(1<<bit) != 0 {
				indices = append(indices, i*8+bit)
			}
		}
	}
	return indices, nil
}

func buildPCRSelection(indices []int) (tpm2.TPMLPCRSelection, error) {
	sel := make([]byte, 3)
	for _, idx := range indices {
		if idx < 0 || idx > 23 {
			return tpm2.TPMLPCRSelection{}, fmt.Errorf("invalid PCR index %d: must be 0-23", idx)
		}
		sel[idx/8] |= 1 << (idx % 8)
	}
	return tpm2.TPMLPCRSelection{
		PCRSelections: []tpm2.TPMSPCRSelection{
			{Hash: tpm2.TPMAlgSHA256, PCRSelect: sel},
		},
	}, nil
}

func marshalECDSASig(r, s *big.Int) []byte {
	der, err := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	if err != nil {
		return nil
	}
	return der
}
