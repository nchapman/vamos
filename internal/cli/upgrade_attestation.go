package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	expectedRepoURL      = "https://github.com/nchapman/vamos"
	expectedWorkflowPath = ".github/workflows/release.yml"
	expectedOIDCIssuer   = "https://token.actions.githubusercontent.com"
	inTotoPayloadType    = "application/vnd.in-toto+json"
	inTotoStatementType  = "https://in-toto.io/Statement/v1"
	slsaPredicateType    = "https://slsa.dev/provenance/v1"
)

// Public Good Sigstore Fulcio Root and Intermediate CA certificates used by
// GitHub Actions `actions/attest-build-provenance` keyless signing.
const sigstoreFulcioRootPEM = `-----BEGIN CERTIFICATE-----
MIIB+DCCAX6gAwIBAgITNVkDZoCiofPDsy7dfm6geLbuhzAKBggqhkjOPQQDAzAq
MRUwEwYDVQQKEwxzaWdzdG9yZS5kZXYxETAPBgNVBAMTCHNpZ3N0b3JlMB4XDTIx
MDMwNzAzMjAyOVoXDTMxMDIyMzAzMjAyOVowKjEVMBMGA1UEChMMc2lnc3RvcmUu
ZGV2MREwDwYDVQQDEwhzaWdzdG9yZTB2MBAGByqGSM49AgEGBSuBBAAiA2IABLSy
A7Ii5k+pNO8ZEWY0ylemWDowOkNa3kL+GZE5Z5GWehL9/A9bRNA3RbrsZ5i0Jcas
taRL7Sp5fp/jD5dxqc/UdTVnlvS16an+2Yfswe/QuLolRUCrcOE2+2iA5+tzd6Nm
MGQwDgYDVR0PAQH/BAQDAgEGMBIGA1UdEwEB/wQIMAYBAf8CAQEwHQYDVR0OBBYE
FMjFHQBBmiQpMlEk6w2uSu1KBtPsMB8GA1UdIwQYMBaAFMjFHQBBmiQpMlEk6w2u
Su1KBtPsMAoGCCqGSM49BAMDA2gAMGUCMH8liWJfMui6vXXBhjDgY4MwslmN/TJx
Ve/83WrFomwmNf056y1X48F9c4m3a3ozXAIxAKjRay5/aj/jsKKGIkmQatjI8uup
Hr/+CxFvaJWmpYqNkLDGRU+9orzh5hI2RrcuaQ==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIB9zCCAXygAwIBAgIUALZNAPFdxHPwjeDloDwyYChAO/4wCgYIKoZIzj0EAwMw
KjEVMBMGA1UEChMMc2lnc3RvcmUuZGV2MREwDwYDVQQDEwhzaWdzdG9yZTAeFw0y
MTEwMDcxMzU2NTlaFw0zMTEwMDUxMzU2NThaMCoxFTATBgNVBAoTDHNpZ3N0b3Jl
LmRldjERMA8GA1UEAxMIc2lnc3RvcmUwdjAQBgcqhkjOPQIBBgUrgQQAIgNiAAT7
XeFT4rb3PQGwS4IajtLk3/OlnpgangaBclYpsYBr5i+4ynB07ceb3LP0OIOZdxex
X69c5iVuyJRQ+Hz05yi+UF3uBWAlHpiS5sh0+H2GHE7SXrk1EC5m1Tr19L9gg92j
YzBhMA4GA1UdDwEB/wQEAwIBBjAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRY
wB5fkUWlZql6zJChkyLQKsXF+jAfBgNVHSMEGDAWgBRYwB5fkUWlZql6zJChkyLQ
KsXF+jAKBggqhkjOPQQDAwNpADBmAjEAj1nHeXZp+13NWBNa+EDsDP8G1WWg1tCM
WP/WHPqpaVo0jhsweNFZgSs0eE7wYI4qAjEA2WB9ot98sIkoF3vZYdd3/VtWB5b9
TNMea7Ix/stJ5TfcLLeABLE4BNJOsQ4vnBHJ
-----END CERTIFICATE-----`

const sigstoreFulcioIntermediatePEM = `-----BEGIN CERTIFICATE-----
MIICGjCCAaGgAwIBAgIUALnViVfnU0brJasmRkHrn/UnfaQwCgYIKoZIzj0EAwMw
KjEVMBMGA1UEChMMc2lnc3RvcmUuZGV2MREwDwYDVQQDEwhzaWdzdG9yZTAeFw0y
MjA0MTMyMDA2MTVaFw0zMTEwMDUxMzU2NThaMDcxFTATBgNVBAoTDHNpZ3N0b3Jl
LmRldjEeMBwGA1UEAxMVc2lnc3RvcmUtaW50ZXJtZWRpYXRlMHYwEAYHKoZIzj0C
AQYFK4EEACIDYgAE8RVS/ysH+NOvuDZyPIZtilgUF9NlarYpAd9HP1vBBH1U5CV7
7LSS7s0ZiH4nE7Hv7ptS6LvvR/STk798LVgMzLlJ4HeIfF3tHSaexLcYpSASr1kS
0N/RgBJz/9jWCiXno3sweTAOBgNVHQ8BAf8EBAMCAQYwEwYDVR0lBAwwCgYIKwYB
BQUHAwMwEgYDVR0TAQH/BAgwBgEB/wIBADAdBgNVHQ4EFgQU39Ppz1YkEZb5qNjp
KFWixi4YZD8wHwYDVR0jBBgwFoAUWMAeX5FFpWapesyQoZMi0CrFxfowCgYIKoZI
zj0EAwMDZwAwZAIwPCsQK4DYiZYDPIaDi5HFKnfxXx6ASSVmERfsynYBiX2X6SJR
nZU84/9DZdnFvvxmAjBOt6QpBlc4J/0DxvkTCqpclvziL6BCCPnjdlIB3Pu3BxsP
mygUY7Ii2zbdCdliiow=
-----END CERTIFICATE-----`

var (
	oidFulcioIssuerV1 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	oidFulcioIssuerV2 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
)

type attestationResponse struct {
	Attestations []struct {
		Bundle sigstoreBundle `json:"bundle"`
	} `json:"attestations"`
}

type sigstoreBundle struct {
	VerificationMaterial struct {
		Certificate struct {
			RawBytes string `json:"rawBytes"`
		} `json:"certificate"`
	} `json:"verificationMaterial"`
	DSSEEnvelope struct {
		PayloadType string `json:"payloadType"`
		Payload     string `json:"payload"`
		Signatures  []struct {
			Sig string `json:"sig"`
		} `json:"signatures"`
	} `json:"dsseEnvelope"`
}

type inTotoStatement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	Predicate struct {
		BuildDefinition struct {
			ExternalParameters struct {
				Workflow struct {
					Ref        string `json:"ref"`
					Repository string `json:"repository"`
					Path       string `json:"path"`
				} `json:"workflow"`
			} `json:"externalParameters"`
		} `json:"buildDefinition"`
	} `json:"predicate"`
}

func defaultFulcioPools() (*x509.CertPool, *x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(sigstoreFulcioRootPEM)) {
		return nil, nil, errors.New("load embedded Sigstore root CA")
	}
	intermediates := x509.NewCertPool()
	if !intermediates.AppendCertsFromPEM([]byte(sigstoreFulcioIntermediatePEM)) {
		return nil, nil, errors.New("load embedded Sigstore intermediate CA")
	}
	return roots, intermediates, nil
}

func verifyReleaseAttestation(ctx context.Context, deps upgradeDeps, currentVersion, version, archiveName, sumsHash, archiveHash string) error {
	roots, intermediates := deps.fulcioRoots, deps.fulcioIntermediates
	if roots == nil {
		var err error
		roots, intermediates, err = defaultFulcioPools()
		if err != nil {
			return err
		}
	}

	url := fmt.Sprintf("%s/attestations/sha256:%s", strings.TrimRight(deps.apiBase, "/"), strings.ToLower(sumsHash))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vamos/"+currentVersion)

	resp, err := deps.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch build provenance attestation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch build provenance attestation: GitHub API returned %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumsBytes+1))
	if err != nil {
		return fmt.Errorf("read build provenance attestation: %w", err)
	}
	if len(data) > maxChecksumsBytes {
		return errors.New("build provenance attestation response exceeds size limit")
	}

	var parsed attestationResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("decode build provenance attestation: %w", err)
	}
	if len(parsed.Attestations) == 0 {
		return errors.New("no build provenance attestations found for release")
	}

	var lastErr error
	for _, entry := range parsed.Attestations {
		err := verifyAttestationBundle(entry.Bundle, version, archiveName, sumsHash, archiveHash, roots, intermediates)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("build provenance verification failed: %w", lastErr)
}

func verifyAttestationBundle(bundle sigstoreBundle, version, archiveName, sumsHash, archiveHash string, roots, intermediates *x509.CertPool) error {
	if bundle.DSSEEnvelope.PayloadType != inTotoPayloadType {
		return fmt.Errorf("unexpected DSSE payload type %q", bundle.DSSEEnvelope.PayloadType)
	}
	certDER, err := base64.StdEncoding.DecodeString(bundle.VerificationMaterial.Certificate.RawBytes)
	if err != nil || len(certDER) == 0 {
		return errors.New("invalid attestation certificate encoding")
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("parse attestation certificate: %w", err)
	}

	// Fulcio certificates are short-lived (10 minutes) and bound to the build
	// timestamp; verify the CA chain at the certificate's issuance window.
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   cert.NotBefore.Add(cert.NotAfter.Sub(cert.NotBefore) / 2),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}); err != nil {
		return fmt.Errorf("verify Fulcio certificate chain: %w", err)
	}

	expectedWorkflowURI := fmt.Sprintf("%s/%s@refs/tags/v%s", expectedRepoURL, expectedWorkflowPath, version)
	matchedSAN := false
	for _, uri := range cert.URIs {
		if uri.String() == expectedWorkflowURI {
			matchedSAN = true
			break
		}
	}
	if !matchedSAN {
		return fmt.Errorf("certificate SAN does not match %s", expectedWorkflowURI)
	}
	if !hasFulcioOIDCIssuer(cert, expectedOIDCIssuer) {
		return errors.New("certificate OIDC issuer does not match GitHub Actions")
	}

	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("attestation certificate does not use an ECDSA public key")
	}

	payloadBytes, err := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
	if err != nil || len(payloadBytes) == 0 {
		return errors.New("invalid DSSE payload encoding")
	}
	pae := dssePAE(bundle.DSSEEnvelope.PayloadType, payloadBytes)
	digest := sha256.Sum256(pae)

	sigVerified := false
	for _, s := range bundle.DSSEEnvelope.Signatures {
		sigBytes, err := base64.StdEncoding.DecodeString(s.Sig)
		if err == nil && ecdsa.VerifyASN1(pub, digest[:], sigBytes) {
			sigVerified = true
			break
		}
	}
	if !sigVerified {
		return errors.New("invalid DSSE signature")
	}

	var stmt inTotoStatement
	if err := json.Unmarshal(payloadBytes, &stmt); err != nil {
		return fmt.Errorf("decode in-toto statement: %w", err)
	}
	if stmt.Type != inTotoStatementType || stmt.PredicateType != slsaPredicateType {
		return errors.New("unexpected in-toto statement or SLSA predicate type")
	}
	wf := stmt.Predicate.BuildDefinition.ExternalParameters.Workflow
	if wf.Repository != expectedRepoURL || wf.Path != expectedWorkflowPath || wf.Ref != "refs/tags/v"+version {
		return fmt.Errorf("provenance workflow mismatch: got %s/%s@%s", wf.Repository, wf.Path, wf.Ref)
	}

	matchedSums := false
	matchedArchive := false
	for _, sub := range stmt.Subject {
		got := strings.ToLower(strings.TrimSpace(sub.Digest["sha256"]))
		if got == "" {
			continue
		}
		switch sub.Name {
		case "SHA256SUMS":
			if got != strings.ToLower(sumsHash) {
				return errors.New("provenance digest mismatch for SHA256SUMS")
			}
			matchedSums = true
		case archiveName:
			if got != strings.ToLower(archiveHash) {
				return fmt.Errorf("provenance digest mismatch for %s", archiveName)
			}
			matchedArchive = true
		}
	}
	if !matchedSums || !matchedArchive {
		return fmt.Errorf("provenance statement does not cover both SHA256SUMS and %s", archiveName)
	}
	return nil
}

func dssePAE(payloadType string, payload []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "DSSEv1 %d %s %d ", len(payloadType), payloadType, len(payload))
	buf.Write(payload)
	return buf.Bytes()
}

func hasFulcioOIDCIssuer(cert *x509.Certificate, want string) bool {
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(oidFulcioIssuerV1) && string(ext.Value) == want {
			return true
		}
		if ext.Id.Equal(oidFulcioIssuerV2) {
			var decoded string
			if rest, err := asn1.Unmarshal(ext.Value, &decoded); err == nil && len(rest) == 0 && decoded == want {
				return true
			}
		}
	}
	return false
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
