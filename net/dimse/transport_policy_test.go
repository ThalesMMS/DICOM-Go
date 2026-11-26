package dimse

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"net"
	"testing"
)

func TestDIMSETransportConvenienceCallersAndStorePolicySnapshot(t *testing.T) {
	sentinel := errors.New("synthetic transport refusal")
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "caller-context")
	calls := 0
	policy := ul.TransportOptions{DialContext: func(received context.Context, network, address string) (net.Conn, error) {
		calls++
		if received.Value(key{}) != "caller-context" || network != "tcp" || address != "logical.test:104" {
			t.Fatal("caller transport contract changed")
		}
		return nil, sentinel
	}}
	_, err := VerifyCEcho(ctx, CEchoVerificationOptions{Address: "logical.test:104", Transport: policy})
	if !errors.Is(err, sentinel) {
		t.Fatalf("echo transport=%v", err)
	}
	_, err = DialRetrieveSCU(ctx, RetrieveDialOptions{Address: "logical.test:104", Transport: policy, Contexts: []ul.PresentationContext{{AbstractSyntaxUID: StudyRootGetSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("retrieve transport=%v", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	original := ul.DialOptions{Transport: ul.TransportOptions{LocalAddress: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}, HTTPConnect: &ul.HTTPConnectOptions{Address: "proxy.test:80", Password: "original"}}}
	frozen := cloneStoreDialOptions(original)
	original.Transport.LocalAddress.IP[0] = 200
	original.Transport.HTTPConnect.Address = "other.test:80"
	original.Transport.HTTPConnect.Password = "mutated"
	if frozen.Transport.LocalAddress.IP.String() != "127.0.0.1" || frozen.Transport.HTTPConnect.Address != "proxy.test:80" || frozen.Transport.HTTPConnect.Password != "original" {
		t.Fatal("store retained mutable caller policy")
	}
}
