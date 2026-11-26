package certrevocation

import (
	"crypto/x509"
	"math/big"
	"sync"
	"testing"
	"time"
)

func TestCRLChecksIssuerSignatureTimeAndConservativeSets(t *testing.T) {
	a := crlCA(t, nil, 1)
	leaf := crlLeaf(t, a)
	other := crlCA(t, nil, 2)
	at := time.Now()
	good := crlList(t, a, nil)
	for _, tc := range []struct {
		name  string
		lists []*x509.RevocationList
		kind  Kind
	}{
		{"good", []*x509.RevocationList{good}, ""},
		{"missing", nil, Missing},
		{"wrong-key-id", []*x509.RevocationList{crlList(t, other, nil)}, IssuerMismatch},
		{"future", []*x509.RevocationList{crlList(t, a, func(r *x509.RevocationList) { r.ThisUpdate = at.Add(time.Hour); r.NextUpdate = at.Add(2 * time.Hour) })}, NotYetValid},
		{"expired", []*x509.RevocationList{crlList(t, a, func(r *x509.RevocationList) { r.ThisUpdate = at.Add(-2 * time.Hour); r.NextUpdate = at.Add(-time.Hour) })}, Expired},
		{"future-revocation-date", []*x509.RevocationList{crlList(t, a, func(r *x509.RevocationList) {
			r.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: leaf.SerialNumber, RevocationTime: at.Add(24 * time.Hour)}}
		})}, Revoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(tc.lists)
			if err != nil {
				t.Fatal(err)
			}
			err = s.Check(leaf, a.cert, at)
			if tc.kind == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireCRLKind(t, err, tc.kind)
			}
		})
	}
	bad := *good
	bad.Raw = append([]byte(nil), good.Raw...)
	bad.Raw[len(bad.Raw)-1] ^= 1
	s, err := New([]*x509.RevocationList{&bad})
	if err != nil {
		t.Fatal(err)
	}
	requireCRLKind(t, s.Check(leaf, a.cert, at), BadSignature)
	noCRLSign := *a.cert
	noCRLSign.KeyUsage &^= x509.KeyUsageCRLSign
	s, _ = New([]*x509.RevocationList{good})
	requireCRLKind(t, s.Check(leaf, &noCRLSign, at), BadSignature)
	revoked := crlList(t, a, func(r *x509.RevocationList) {
		r.Number = big.NewInt(2)
		r.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: leaf.SerialNumber, RevocationTime: at}}
	})
	for _, lists := range [][]*x509.RevocationList{{good, revoked}, {revoked, good}} {
		s, _ := New(lists)
		requireCRLKind(t, s.Check(leaf, a.cert, at), Revoked)
	}
	staleRevoked := crlList(t, a, func(r *x509.RevocationList) {
		r.ThisUpdate = at.Add(-2 * time.Hour)
		r.NextUpdate = at.Add(-time.Hour)
		r.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: leaf.SerialNumber, RevocationTime: at.Add(-3 * time.Hour)}}
	})
	s, _ = New([]*x509.RevocationList{staleRevoked, good})
	if err := s.Check(leaf, a.cert, at); err != nil {
		t.Fatal(err)
	}
}

func TestCRLChainAndDetachedConcurrentSnapshot(t *testing.T) {
	root := crlCA(t, nil, 1)
	inter := crlCA(t, &root, 2)
	leaf := crlLeaf(t, inter)
	leafList := crlList(t, inter, nil)
	rootList := crlList(t, root, func(r *x509.RevocationList) {
		r.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: inter.cert.SerialNumber, RevocationTime: r.ThisUpdate}}
	})
	s, err := New([]*x509.RevocationList{leafList, rootList})
	if err != nil {
		t.Fatal(err)
	}
	chain := [][]*x509.Certificate{{leaf, inter.cert, root.cert}}
	if err = s.CheckChains(chain, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	requireCRLKind(t, s.CheckChains(chain, true, time.Now()), Revoked)
	leafOnly, _ := New([]*x509.RevocationList{leafList})
	requireCRLKind(t, leafOnly.CheckChains(chain, true, time.Now()), IssuerMismatch)
	// A self-signed trust anchor used as the peer leaf is not excluded.
	anchorList := crlList(t, root, func(r *x509.RevocationList) {
		r.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: root.cert.SerialNumber, RevocationTime: r.ThisUpdate}}
	})
	anchorSet, _ := New([]*x509.RevocationList{anchorList})
	requireCRLKind(t, anchorSet.CheckChains([][]*x509.Certificate{{root.cert}}, true, time.Now()), Revoked)
	// Mutating every public parsed field and Raw after compilation cannot
	// change the signed snapshot, including while readers run concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := leafOnly.CheckChains(chain, false, time.Now()); err != nil {
					t.Errorf("snapshot: %v", err)
				}
			}
		}()
	}
	for i := range leafList.Raw {
		leafList.Raw[i] = 0
	}
	leafList.NextUpdate = time.Time{}
	leafList.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: leaf.SerialNumber}}
	wg.Wait()
}
