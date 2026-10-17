//go:build linux

package tpm

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
)

// Attestation follows the TCG credential activation flow:
//
//  1. The device sends EKPublic and AKPublic to the verifier. The verifier
//     checks the EK (for example against the EK certificate of the TPM
//     vendor) and calls MakeCredential with a random secret.
//  2. The device calls ActivateCredential and returns the secret. Only the
//     TPM that holds both the EK and an AK with exactly that public area can
//     decrypt it, so the verifier now trusts AKPublic.
//  3. The verifier sends a fresh nonce, the device calls Quote, and the
//     verifier calls VerifyQuote with the trusted AKPublic and the nonce.
//
// The EK and the AK are primary keys of the endorsement hierarchy. The TPM
// derives them from the endorsement seed and the template, so they are the
// same after each boot and BOOTy does not store key blobs.

// akTemplate is a restricted ECDSA P-256 signing key: it signs only data
// that the TPM generated itself (quotes, certifications), never an external
// digest that looks like a quote.
var akTemplate = tpm2.TPMTPublic{
	Type:    tpm2.TPMAlgECC,
	NameAlg: tpm2.TPMAlgSHA256,
	ObjectAttributes: tpm2.TPMAObject{
		FixedTPM:            true,
		FixedParent:         true,
		SensitiveDataOrigin: true,
		UserWithAuth:        true,
		Restricted:          true,
		SignEncrypt:         true,
	},
	Parameters: tpm2.NewTPMUPublicParms(tpm2.TPMAlgECC,
		&tpm2.TPMSECCParms{
			Symmetric: tpm2.TPMTSymDefObject{Algorithm: tpm2.TPMAlgNull},
			Scheme: tpm2.TPMTECCScheme{
				Scheme: tpm2.TPMAlgECDSA,
				Details: tpm2.NewTPMUAsymScheme(tpm2.TPMAlgECDSA,
					&tpm2.TPMSSigSchemeECDSA{HashAlg: tpm2.TPMAlgSHA256}),
			},
			CurveID: tpm2.TPMECCNistP256,
			KDF:     tpm2.TPMTKDFScheme{Scheme: tpm2.TPMAlgNull},
		}),
	Unique: tpm2.NewTPMUPublicID(tpm2.TPMAlgECC, &tpm2.TPMSECCPoint{}),
}

// ekTemplate is the TCG reference RSA-2048 EK template (low range), which
// every TPM 2.0 supports and for which vendors provision EK certificates.
// ponytail: RSA EK only; add the ECC P-256 EK template when a fleet TPM
// ships only an ECC EK certificate.
var ekTemplate = tpm2.RSAEKTemplate

// key is a loaded primary key.
type key struct {
	handle tpm2.TPMHandle
	name   tpm2.TPM2BName
	public tpm2.TPM2BPublic
}

func (d *Device) createPrimary(hierarchy tpm2.TPMHandle, template *tpm2.TPMTPublic) (*key, error) {
	rsp, err := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.AuthHandle{Handle: hierarchy, Auth: tpm2.PasswordAuth(nil)},
		InPublic:      tpm2.New2B(*template),
	}.Execute(d.tpm)
	if err != nil {
		return nil, fmt.Errorf("TPM2_CreatePrimary: %w", err)
	}
	return &key{handle: rsp.ObjectHandle, name: rsp.Name, public: rsp.OutPublic}, nil
}

func (d *Device) withPrimary(template *tpm2.TPMTPublic, what string, fn func(*key) error) error {
	k, err := d.createPrimary(tpm2.TPMRHEndorsement, template)
	if err != nil {
		return fmt.Errorf("creating %s: %w", what, err)
	}
	defer d.flushContext(k.handle)
	return fn(k)
}

// EKPublic returns the TPMT_PUBLIC area of the RSA endorsement key.
func (d *Device) EKPublic() ([]byte, error) {
	var pub []byte
	err := d.withPrimary(&ekTemplate, "EK", func(k *key) error {
		pub = k.public.Bytes()
		return nil
	})
	return pub, err
}

// AKPublic returns the TPMT_PUBLIC area of the attestation key.
func (d *Device) AKPublic() ([]byte, error) {
	var pub []byte
	err := d.withPrimary(&akTemplate, "AK", func(k *key) error {
		pub = k.public.Bytes()
		return nil
	})
	return pub, err
}

// ActivateCredential decrypts a MakeCredential challenge with the EK and
// returns the secret. The TPM releases it only when the challenge names
// the AK of this TPM.
func (d *Device) ActivateCredential(credentialBlob, encryptedSecret []byte) ([]byte, error) {
	var secret []byte
	err := d.withPrimary(&ekTemplate, "EK", func(ek *key) error {
		return d.withPrimary(&akTemplate, "AK", func(ak *key) error {
			rsp, err := tpm2.ActivateCredential{
				ActivateHandle: tpm2.AuthHandle{Handle: ak.handle, Name: ak.name, Auth: tpm2.PasswordAuth(nil)},
				KeyHandle: tpm2.AuthHandle{
					Handle: ek.handle,
					Name:   ek.name,
					// The EK policy is PolicySecret(TPM_RH_ENDORSEMENT).
					Auth: tpm2.Policy(tpm2.TPMAlgSHA256, 16, endorsementPolicy),
				},
				CredentialBlob: tpm2.TPM2BIDObject{Buffer: credentialBlob},
				Secret:         tpm2.TPM2BEncryptedSecret{Buffer: encryptedSecret},
			}.Execute(d.tpm)
			if err != nil {
				return fmt.Errorf("TPM2_ActivateCredential: %w", err)
			}
			secret = rsp.CertInfo.Buffer
			return nil
		})
	})
	return secret, err
}

func endorsementPolicy(t transport.TPM, session tpm2.TPMISHPolicy, nonceTPM tpm2.TPM2BNonce) error {
	_, err := tpm2.PolicySecret{
		AuthHandle:    tpm2.TPMRHEndorsement,
		PolicySession: session,
		NonceTPM:      nonceTPM,
	}.Execute(t)
	if err != nil {
		return fmt.Errorf("TPM2_PolicySecret: %w", err)
	}
	return nil
}

// Quote signs the SHA-256 bank of the PCRs with the AK. nonce is the fresh
// qualifying data of the verifier. PCRValues are read after the quote for
// the verifier; VerifyQuote checks them against the signed PCR digest.
func (d *Device) Quote(pcrIndices []int, nonce []byte) (*AttestationQuote, error) {
	if len(pcrIndices) == 0 {
		return nil, errors.New("no PCR indices specified")
	}
	if len(nonce) == 0 {
		return nil, errors.New("empty nonce")
	}
	sel, err := buildPCRSelection(pcrIndices)
	if err != nil {
		return nil, err
	}
	var q *AttestationQuote
	err = d.withPrimary(&akTemplate, "AK", func(ak *key) error {
		rsp, err := tpm2.Quote{
			SignHandle:     tpm2.AuthHandle{Handle: ak.handle, Name: ak.name, Auth: tpm2.PasswordAuth(nil)},
			QualifyingData: tpm2.TPM2BData{Buffer: nonce},
			InScheme: tpm2.TPMTSigScheme{
				Scheme:  tpm2.TPMAlgECDSA,
				Details: tpm2.NewTPMUSigScheme(tpm2.TPMAlgECDSA, &tpm2.TPMSSchemeHash{HashAlg: tpm2.TPMAlgSHA256}),
			},
			PCRSelect: sel,
		}.Execute(d.tpm)
		if err != nil {
			return fmt.Errorf("TPM2_Quote: %w", err)
		}
		sig, err := rsp.Signature.Signature.ECDSA()
		if err != nil {
			return fmt.Errorf("quote signature: %w", err)
		}
		q = &AttestationQuote{
			QuoteData: rsp.Quoted.Bytes(),
			Signature: marshalECDSASig(new(big.Int).SetBytes(sig.SignatureR.Buffer), new(big.Int).SetBytes(sig.SignatureS.Buffer)),
			PCRValues: make(map[int][]byte, len(pcrIndices)),
			Nonce:     nonce,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, idx := range pcrIndices {
		val, err := d.ReadPCR(idx)
		if err != nil {
			return nil, fmt.Errorf("reading PCR %d: %w", idx, err)
		}
		q.PCRValues[idx] = val
	}
	return q, nil
}

func (d *Device) flushContext(handle tpm2.TPMHandle) {
	cmd := tpm2.FlushContext{FlushHandle: handle}
	_, _ = cmd.Execute(d.tpm)
}
