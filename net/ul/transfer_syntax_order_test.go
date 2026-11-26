package ul

import "testing"

func TestAcceptorTransferSyntaxOrder(t *testing.T) {
	const (
		rle      = "1.2.840.10008.1.2.5"
		explicit = "1.2.840.10008.1.2.1"
		implicit = "1.2.840.10008.1.2"
		jpegls   = "1.2.840.10008.1.2.4.80"
	)
	proposed := []PresentationContextProposed{
		{ID: 1, AbstractSyntaxUID: "1.2.840.10008.5.1.4.1.1.7", TransferSyntaxUIDs: []string{rle, explicit, implicit}},
		{ID: 3, AbstractSyntaxUID: "1.2.840.10008.5.1.4.1.1.7", TransferSyntaxUIDs: []string{jpegls}},
	}
	supported := []string{explicit, implicit, rle}
	for _, tc := range []struct {
		name          string
		proposerOrder bool
		want          []string
	}{
		{"acceptor order by default", false, []string{explicit, ""}},
		{"proposer order when requested", true, []string{rle, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, accepted := negotiatePresentationContexts(proposed, AcceptOptions{SupportedAbstractSyntaxes: []string{"1.2.840.10008.5.1.4.1.1.7"}, SupportedTransferSyntaxes: supported, PreferProposedTransferSyntaxOrder: tc.proposerOrder})
			if len(accepted) != 1 || accepted[0].TransferSyntaxUID != tc.want[0] {
				t.Fatalf("accepted = %+v, want %s", accepted, tc.want[0])
			}
			if results[1].Result != PresentationContextTransferSyntaxesNotSupported {
				t.Fatalf("unsupported-only context result = %d", results[1].Result)
			}
		})
	}
}
