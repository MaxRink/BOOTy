//go:build linux

package tpm

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-tpm/tpm2"
)

// streamTPM sends raw TPM commands over a stream socket, like the swtpm
// "socket --server" data channel, and reads each whole response.
type streamTPM struct{ conn net.Conn }

func (s *streamTPM) Send(cmd []byte) ([]byte, error) {
	if _, err := s.conn.Write(cmd); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	hdr := make([]byte, 10) // tag, size, response code
	if _, err := io.ReadFull(s.conn, hdr); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	size := binary.BigEndian.Uint32(hdr[2:6])
	if size < 10 || size > 1<<20 {
		return nil, fmt.Errorf("bad response size %d", size)
	}
	rsp := make([]byte, size)
	copy(rsp, hdr)
	if _, err := io.ReadFull(s.conn, rsp[10:]); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return rsp, nil
}

func (s *streamTPM) Close() error { return s.conn.Close() }

// openSWTPM starts swtpm with a fresh state (its own endorsement seed) and
// returns a Device on it. Without swtpm the test is skipped; CI fails on
// skipped tests.
func openSWTPM(t *testing.T) *Device {
	t.Helper()
	if _, err := exec.LookPath("swtpm"); err != nil {
		t.Skip("swtpm not installed")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "tpm.sock")
	cmd := exec.CommandContext(context.Background(), "swtpm", "socket", "--tpm2",
		"--tpmstate", "dir="+dir,
		"--server", "type=unixio,path="+sock,
		"--ctrl", "type=unixio,path="+filepath.Join(dir, "ctrl.sock"),
		"--flags", "not-need-init,startup-clear")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start swtpm: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var conn net.Conn
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var err error
		if conn, err = (&net.Dialer{}).DialContext(context.Background(), "unix", sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("swtpm socket: %v; stderr: %s", err, stderr.String())
		}
	}
	d := &Device{tpm: retryTPM{&streamTPM{conn: conn}}}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// activatedAK runs credential activation and returns the AK public area
// that a verifier trusts afterwards.
func activatedAK(t *testing.T, d *Device) []byte {
	t.Helper()
	ek, err := d.EKPublic()
	if err != nil {
		t.Fatalf("EKPublic: %v", err)
	}
	ak, err := d.AKPublic()
	if err != nil {
		t.Fatalf("AKPublic: %v", err)
	}
	secret := randomBytes(t, 32)
	blob, enc, err := MakeCredential(ek, ak, secret)
	if err != nil {
		t.Fatalf("MakeCredential: %v", err)
	}
	got, err := d.ActivateCredential(blob, enc)
	if err != nil {
		t.Fatalf("ActivateCredential: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("ActivateCredential returned %x, want the secret %x", got, secret)
	}
	return ak
}

func TestAttestationSWTPM(t *testing.T) {
	d := openSWTPM(t)
	ak := activatedAK(t, d)

	// BOOTy extends its events into PCR 23; the verifier replays its log.
	log := NewMeasurementLog()
	for _, event := range []string{"booty binary", "provision config"} {
		digest := HashBytes([]byte(event))
		if err := d.ExtendPCR(PCRBooty, digest); err != nil {
			t.Fatalf("ExtendPCR: %v", err)
		}
		log.Add(PCRBooty, event, digest)
	}
	// The BOOTy boot chain (shim and GRUB in 4, GRUB commands in 8, kernel
	// and initramfs in 9) and the BOOTy events (23).
	pcrs := []int{PCRBootLoader, PCRGRUBCmds, PCRGRUBFiles, PCRBooty}
	nonce := randomBytes(t, 32)
	quote, err := d.Quote(pcrs, nonce)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	t.Run("valid quote", func(t *testing.T) {
		if err := VerifyQuote(ak, quote, nonce); err != nil {
			t.Fatalf("VerifyQuote: %v", err)
		}
		want := ReplayMeasurementLog(log)
		if ok, mismatches := VerifyPCRs(want, quote.PCRValues); !ok {
			t.Fatalf("PCR 23 does not match the replayed log: %v", mismatches)
		}
	})

	t.Run("AK is a primary key of the endorsement hierarchy", func(t *testing.T) {
		pub, err := tpm2.Unmarshal[tpm2.TPMTPublic](ak)
		if err != nil {
			t.Fatal(err)
		}
		name, err := tpm2.ObjectName(pub)
		if err != nil {
			t.Fatal(err)
		}
		// Qualified name: nameAlg || SHA-256(handle of TPM_RH_ENDORSEMENT || name).
		h := sha256.New()
		h.Write(binary.BigEndian.AppendUint32(nil, uint32(tpm2.TPMRHEndorsement)))
		h.Write(name.Buffer)
		want := append(binary.BigEndian.AppendUint16(nil, uint16(tpm2.TPMAlgSHA256)), h.Sum(nil)...)
		attest, err := tpm2.Unmarshal[tpm2.TPMSAttest](quote.QuoteData)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(attest.QualifiedSigner.Buffer, want) {
			t.Fatalf("quote signer %x is not the AK under the endorsement hierarchy (%x)", attest.QualifiedSigner.Buffer, want)
		}
	})

	t.Run("AK is stable", func(t *testing.T) {
		again, err := d.AKPublic()
		if err != nil || !bytes.Equal(again, ak) {
			t.Fatalf("AKPublic changed between calls (%v)", err)
		}
	})

	t.Run("stale nonce", func(t *testing.T) {
		if err := VerifyQuote(ak, quote, randomBytes(t, 32)); !errors.Is(err, ErrQuoteNonce) {
			t.Fatalf("VerifyQuote with another nonce = %v, want ErrQuoteNonce", err)
		}
		// The nonce field of the quote is not signed and does not count.
		stale := *quote
		stale.Nonce = nil
		if err := VerifyQuote(ak, &stale, nonce); err != nil {
			t.Fatalf("VerifyQuote ignores the nonce field: %v", err)
		}
	})

	t.Run("wrong PCR value", func(t *testing.T) {
		forged := *quote
		forged.PCRValues = maps.Clone(quote.PCRValues)
		forged.PCRValues[PCRBooty] = HashBytes([]byte("forged"))
		if err := VerifyQuote(ak, &forged, nonce); !errors.Is(err, ErrQuotePCRMismatch) {
			t.Fatalf("VerifyQuote with a forged PCR 23 = %v, want ErrQuotePCRMismatch", err)
		}
	})

	t.Run("PCR selection differs from the values", func(t *testing.T) {
		partial := *quote
		partial.PCRValues = map[int][]byte{PCRBooty: quote.PCRValues[PCRBooty]}
		if err := VerifyQuote(ak, &partial, nonce); !errors.Is(err, ErrQuotePCRMismatch) {
			t.Fatalf("VerifyQuote with a dropped PCR = %v, want ErrQuotePCRMismatch", err)
		}
	})

	t.Run("unsigned extra PCR value", func(t *testing.T) {
		extra := *quote
		extra.PCRValues = maps.Clone(quote.PCRValues)
		extra.PCRValues[PCRSecureBoot] = HashBytes([]byte("not quoted"))
		if err := VerifyQuote(ak, &extra, nonce); !errors.Is(err, ErrQuotePCRMismatch) {
			t.Fatalf("VerifyQuote with a PCR outside the quote = %v, want ErrQuotePCRMismatch", err)
		}
	})

	t.Run("golden policy mismatch", func(t *testing.T) {
		policy := &GoldenPolicy{PCRs: []GoldenPCR{{PCR: PCRBooty, Algorithm: "sha256", Digest: hex.EncodeToString(HashBytes([]byte("other")))}}}
		if r := VerifyQuoteAgainstPolicy(quote.PCRValues, policy); r.Valid {
			t.Fatal("a wrong golden PCR 23 passed")
		}
	})

	t.Run("tampered quote data", func(t *testing.T) {
		tampered := *quote
		tampered.QuoteData = bytes.Clone(quote.QuoteData)
		tampered.QuoteData[len(tampered.QuoteData)-1] ^= 1
		if err := VerifyQuote(ak, &tampered, nonce); !errors.Is(err, ErrQuoteSignature) {
			t.Fatalf("VerifyQuote with tampered data = %v, want ErrQuoteSignature", err)
		}
	})
}

// A quote without qualifying data must not pass with an empty expected
// nonce: it would be a replayable quote.
func TestVerifyQuoteRejectsEmptyNonceSWTPM(t *testing.T) {
	d := openSWTPM(t)
	ak := activatedAK(t, d)
	sel, err := buildPCRSelection([]int{PCRBooty})
	if err != nil {
		t.Fatal(err)
	}
	var quote *AttestationQuote
	err = d.withPrimary(&akTemplate, "AK", func(k *key) error {
		rsp, err := tpm2.Quote{
			SignHandle: tpm2.AuthHandle{Handle: k.handle, Name: k.name, Auth: tpm2.PasswordAuth(nil)},
			InScheme: tpm2.TPMTSigScheme{
				Scheme:  tpm2.TPMAlgECDSA,
				Details: tpm2.NewTPMUSigScheme(tpm2.TPMAlgECDSA, &tpm2.TPMSSchemeHash{HashAlg: tpm2.TPMAlgSHA256}),
			},
			PCRSelect: sel,
		}.Execute(d.tpm)
		if err != nil {
			return err
		}
		sig, err := rsp.Signature.Signature.ECDSA()
		if err != nil {
			return err
		}
		quote = &AttestationQuote{
			QuoteData: rsp.Quoted.Bytes(),
			Signature: marshalECDSASig(new(big.Int).SetBytes(sig.SignatureR.Buffer), new(big.Int).SetBytes(sig.SignatureS.Buffer)),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pcr, err := d.ReadPCR(PCRBooty)
	if err != nil {
		t.Fatal(err)
	}
	quote.PCRValues = map[int][]byte{PCRBooty: pcr}
	if err := VerifyQuote(ak, quote, []byte{}); err == nil {
		t.Fatal("VerifyQuote accepted a quote without a nonce")
	}
}

// A quote from another TPM does not verify with the trusted AK, and that
// TPM cannot activate a credential for an AK that is not its own.
func TestAttestationForeignKeySWTPM(t *testing.T) {
	d := openSWTPM(t)
	foreign := openSWTPM(t)
	ak := activatedAK(t, d)

	nonce := randomBytes(t, 32)
	quote, err := foreign.Quote([]int{PCRBooty}, nonce)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if err := VerifyQuote(ak, quote, nonce); !errors.Is(err, ErrQuoteSignature) {
		t.Fatalf("VerifyQuote of a foreign quote = %v, want ErrQuoteSignature", err)
	}

	foreignAK, err := foreign.AKPublic()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyQuote(foreignAK, quote, nonce); err != nil {
		t.Fatalf("the foreign quote does not verify with its own AK: %v", err)
	}
	ek, err := d.EKPublic()
	if err != nil {
		t.Fatal(err)
	}
	// A credential for the foreign AK, encrypted to the EK of d: d has no
	// object with that name.
	blob, enc, err := MakeCredential(ek, foreignAK, randomBytes(t, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ActivateCredential(blob, enc); err == nil {
		t.Fatal("ActivateCredential released a secret for a foreign AK")
	}
	// A credential encrypted to the foreign EK cannot be activated by d.
	foreignEK, err := foreign.EKPublic()
	if err != nil {
		t.Fatal(err)
	}
	ak2, _ := d.AKPublic()
	blob, enc, err = MakeCredential(foreignEK, ak2, randomBytes(t, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ActivateCredential(blob, enc); err == nil {
		t.Fatal("ActivateCredential decrypted a credential for a foreign EK")
	}
}

// The verifier rejects an AK that is not a restricted signing key: such a
// key can sign a forged TPMS_ATTEST.
func TestVerifierRejectsUnrestrictedAK(t *testing.T) {
	otherScheme := func(curve tpm2.TPMECCCurve, hash tpm2.TPMIAlgHash) func(*tpm2.TPMTPublic) {
		return func(p *tpm2.TPMTPublic) {
			p.Parameters = tpm2.NewTPMUPublicParms(tpm2.TPMAlgECC, &tpm2.TPMSECCParms{
				Symmetric: tpm2.TPMTSymDefObject{Algorithm: tpm2.TPMAlgNull},
				Scheme: tpm2.TPMTECCScheme{
					Scheme:  tpm2.TPMAlgECDSA,
					Details: tpm2.NewTPMUAsymScheme(tpm2.TPMAlgECDSA, &tpm2.TPMSSigSchemeECDSA{HashAlg: hash}),
				},
				CurveID: curve,
				KDF:     tpm2.TPMTKDFScheme{Scheme: tpm2.TPMAlgNull},
			})
		}
	}
	for name, mutate := range map[string]func(*tpm2.TPMTPublic){
		"RSA key": func(p *tpm2.TPMTPublic) {
			*p = tpm2.RSASRKTemplate
			p.ObjectAttributes.Decrypt, p.ObjectAttributes.SignEncrypt = false, true
		},
		"not a signing key": func(p *tpm2.TPMTPublic) { p.ObjectAttributes.SignEncrypt = false },
		"P-384 curve":       otherScheme(tpm2.TPMECCNistP384, tpm2.TPMAlgSHA256),
		"SHA-1 scheme":      otherScheme(tpm2.TPMECCNistP256, tpm2.TPMAlgSHA1),
		"not restricted":    func(p *tpm2.TPMTPublic) { p.ObjectAttributes.Restricted = false },
		"not fixedTPM":      func(p *tpm2.TPMTPublic) { p.ObjectAttributes.FixedTPM = false },
		"decrypt key":       func(p *tpm2.TPMTPublic) { p.ObjectAttributes.Decrypt = true },
		"imported key":      func(p *tpm2.TPMTPublic) { p.ObjectAttributes.SensitiveDataOrigin = false },
		"not fixed parent":  func(p *tpm2.TPMTPublic) { p.ObjectAttributes.FixedParent = false },
	} {
		t.Run(name, func(t *testing.T) {
			pub := akTemplate
			mutate(&pub)
			raw := tpm2.Marshal(pub)
			if err := VerifyQuote(raw, &AttestationQuote{}, []byte("n")); !errors.Is(err, ErrAKNotRestricted) {
				t.Fatalf("VerifyQuote = %v, want ErrAKNotRestricted", err)
			}
			if _, _, err := MakeCredential(nil, raw, []byte("s")); !errors.Is(err, ErrAKNotRestricted) {
				t.Fatalf("MakeCredential = %v, want ErrAKNotRestricted", err)
			}
		})
	}
}

func TestSHA256SelectionOnly(t *testing.T) {
	sel := tpm2.TPMLPCRSelection{PCRSelections: []tpm2.TPMSPCRSelection{{Hash: tpm2.TPMAlgSHA1, PCRSelect: []byte{0, 0, 0x80}}}}
	if _, err := sha256Selection(sel); !errors.Is(err, ErrQuotePCRMismatch) {
		t.Fatalf("sha256Selection(SHA-1 bank) = %v, want ErrQuotePCRMismatch", err)
	}
	sel.PCRSelections[0].Hash = tpm2.TPMAlgSHA256
	sel.PCRSelections[0].PCRSelect = []byte{0x81, 0, 0x80}
	got, err := sha256Selection(sel)
	if err != nil || !slices.Equal(got, []int{0, 7, 23}) {
		t.Fatalf("sha256Selection = %v, %v; want [0 7 23]", got, err)
	}
}

func TestQuoteRequiresNonce(t *testing.T) {
	d := &Device{}
	if _, err := d.Quote([]int{PCRBooty}, nil); err == nil {
		t.Fatal("Quote without a nonce succeeded")
	}
	if err := VerifyQuote(nil, &AttestationQuote{}, nil); err == nil {
		t.Fatal("VerifyQuote without an expected nonce succeeded")
	}
}

// MakeCredential refuses an EK public area that is not a TCG EK: a key the
// device controls could decrypt the credential without the TPM.
func TestMakeCredentialRejectsNonTCGEK(t *testing.T) {
	d := openSWTPM(t)
	ek, err := d.EKPublic()
	if err != nil {
		t.Fatal(err)
	}
	ak, err := d.AKPublic()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := MakeCredential(ek, ak, []byte("s")); err != nil {
		t.Fatalf("MakeCredential with the TPM EK: %v", err)
	}
	for name, mutate := range map[string]func(*tpm2.TPMTPublic){
		"not restricted":       func(p *tpm2.TPMTPublic) { p.ObjectAttributes.Restricted = false },
		"not a decryption key": func(p *tpm2.TPMTPublic) { p.ObjectAttributes.Decrypt = false },
		"signing key":          func(p *tpm2.TPMTPublic) { p.ObjectAttributes.SignEncrypt = true },
		"not fixedTPM":         func(p *tpm2.TPMTPublic) { p.ObjectAttributes.FixedTPM = false },
		"not fixed parent":     func(p *tpm2.TPMTPublic) { p.ObjectAttributes.FixedParent = false },
		"imported key":         func(p *tpm2.TPMTPublic) { p.ObjectAttributes.SensitiveDataOrigin = false },
		"no auth policy":       func(p *tpm2.TPMTPublic) { p.AuthPolicy = tpm2.TPM2BDigest{} },
		"another auth policy":  func(p *tpm2.TPMTPublic) { p.AuthPolicy = tpm2.TPM2BDigest{Buffer: make([]byte, 32)} },
	} {
		t.Run(name, func(t *testing.T) {
			pub, err := tpm2.Unmarshal[tpm2.TPMTPublic](ek)
			if err != nil {
				t.Fatal(err)
			}
			mutate(pub)
			if _, _, err := MakeCredential(tpm2.Marshal(*pub), ak, []byte("s")); !errors.Is(err, ErrEKNotTCG) {
				t.Fatalf("MakeCredential = %v, want ErrEKNotTCG", err)
			}
		})
	}
}

// parseAK takes the P-256 point of the AK public area and rejects a point
// that is not on the curve.
func TestParseAKPoint(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	withPoint := func(x, y []byte) []byte {
		pub := akTemplate
		pub.Unique = tpm2.NewTPMUPublicID(tpm2.TPMAlgECC, &tpm2.TPMSECCPoint{
			X: tpm2.TPM2BECCParameter{Buffer: x}, Y: tpm2.TPM2BECCParameter{Buffer: y},
		})
		return tpm2.Marshal(pub)
	}
	got, _, err := parseAK(withPoint(b[1:33], b[33:]))
	if err != nil || !got.Equal(&key.PublicKey) {
		t.Fatalf("parseAK = %v, %v; want the generated key", got, err)
	}
	if _, _, err := parseAK(withPoint(b[1:33], []byte{1})); err == nil {
		t.Fatal("parseAK accepted a point off the curve")
	}
}
