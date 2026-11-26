package retrievecli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestRetrieveKeysSupplyRequiredUIDsWithoutFlags(t *testing.T) {
	opts, err := parseRetrieveArgs([]string{"-k", "QueryRetrieveLevel=SERIES", "-k", "StudyInstanceUID=1.2.3", "-k", "(0020,000e)=1.2.3.4"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := buildRetrieveQueries(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatal("wrong query count")
	}
	assertIdentifierStrings(t, ids[0], map[core.Tag]string{tagQueryRetrieveLevel: "SERIES", tagStudyInstanceUID: "1.2.3", tagSeriesInstanceUID: "1.2.3.4"})
	if code := Run([]string{"-k", "StudyInstanceUID=invalid"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("invalid UID exit=%d", code)
	}
}

func TestRetrieveQueryDecodeErrorOmitsInputAndDoesNotDial(t *testing.T) {
	const marker = "SYN_SECRET_1355"
	path := filepath.Join(t.TempDir(), "query.json")
	if err := os.WriteFile(path, []byte(`{"00280010":{"vr":"US","Value":["`+marker+`"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var stderr bytes.Buffer
	if code := Run([]string{"-remote", listener.Addr().String(), path}, io.Discard, &stderr); code != 2 {
		t.Fatalf("query decode exit=%d", code)
	}
	if strings.Contains(stderr.String(), marker) {
		t.Fatal("query decoder diagnostic exposes input payload")
	}
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
	if conn, err := listener.Accept(); err == nil {
		conn.Close()
		t.Fatal("invalid query dialed the remote")
	}
}

func TestRetrieveQueryFileRejectsWrongLevelVR(t *testing.T) {
	id := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tagQueryRetrieveLevel, VR: core.VRLO}, Value: core.StringValue{"STUDY"}},
		{Header: core.ElementHeader{Tag: tagStudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.3"}},
	}, std.Dictionary)
	for _, format := range []string{"raw", "json"} {
		var data []byte
		if format == "raw" {
			var buf bytes.Buffer
			if err := object.WriteDataSet(&buf, id, transfer.ExplicitVRLittleEndian); err != nil {
				t.Fatal(err)
			}
			data = buf.Bytes()
		} else {
			var err error
			data, err = dicomjson.Marshal(id, dicomjson.Options{})
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, explicitLevel := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%t", format, explicitLevel), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "query.ds")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{path}
				if explicitLevel {
					args = append([]string{"-level", "STUDY"}, args...)
				}
				opts, err := parseRetrieveArgs(args, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := buildRetrieveQueries(context.Background(), opts); err == nil {
					t.Fatal("query-file QueryRetrieveLevel with LO admitted")
				}
				if code := Run(args, io.Discard, io.Discard); code != 2 {
					t.Fatalf("wrong-VR query-file exit=%d, want preflight usage refusal", code)
				}
			})
		}
	}
}
func TestRetrieveQueryFileOverridesAndLevelConflict(t *testing.T) {
	id := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: tagQueryRetrieveLevel, VR: core.VRCS}, Value: core.StringValue{"STUDY"}}, {Header: core.ElementHeader{Tag: tagStudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.3"}}}, std.Dictionary)
	var buf bytes.Buffer
	if err := object.WriteDataSet(&buf, id, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "query.ds")
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	opts, err := parseRetrieveArgs([]string{"-study-uid", "1.2.4", "-k", "StudyInstanceUID=1.2.5", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := buildRetrieveQueries(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifierStrings(t, ids[0], map[core.Tag]string{tagStudyInstanceUID: "1.2.5"})
	opts, err = parseRetrieveArgs([]string{"-level", "SERIES", "-k", "QueryRetrieveLevel=STUDY", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = buildRetrieveQueries(context.Background(), opts); err == nil {
		t.Fatal("explicit level conflict admitted")
	}
}
