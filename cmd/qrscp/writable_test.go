package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

func startWritableQRSCP(t testing.TB, opts options) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, opts, io.Discard, func(address string) { ready <- address }) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && ctx.Err() == nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("qrscp shutdown timeout")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case address := <-ready:
		return address, stop
	case err := <-done:
		cancel()
		once.Do(func() {})
		t.Fatalf("serve %v", err)
	case <-time.After(10 * time.Second):
		stop()
		t.Fatal("qrscp readiness timeout")
	}
	return "", stop
}

func networkStore(t testing.TB, address, uid string) uint16 {
	t.Helper()
	assoc := dial(t, address, []ul.PresentationContext{{AbstractSyntaxUID: dimsetest.SecondaryCaptureSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}}, nil)
	client := dimse.NewStoreClient(assoc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := client.Store(ctx, writableData(t, uid).Dataset)
	if response == nil {
		t.Fatalf("C-STORE no response %v", err)
	}
	_ = assoc.Release(ctx)
	return response.Status
}

func TestWritableStoreFindMoveGetAndRestart(t *testing.T) {
	var moved received
	port := dimsetest.StartSCP(t, ul.AcceptOptions{AETitle: "DEST", SupportedAbstractSyntaxes: []string{dimsetest.SecondaryCaptureSOPClassUID}, SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian}}, func(ctx context.Context, assoc *ul.Association) error {
		return dimse.ServeStorageAssociation(ctx, assoc, dimse.StorageSCPOptions{StoreHandler: dimse.CStoreHandlerFunc(func(_ context.Context, request dimse.CStoreRequestContext) (uint16, error) {
			pixel, ok := request.DataSet.Get(core.NewTag(0x7FE0, 0x0010))
			raw, _ := pixel.RawBytes()
			if !ok || !bytes.Equal(raw, []byte{0, 64, 128, 255}) {
				t.Error("C-MOVE pixel bytes differ")
			}
			moved.add(request.Request.AffectedSOPInstanceUID)
			return dimse.StatusSuccess, nil
		})})
	})
	destination := net.JoinHostPort("127.0.0.1", port)
	opts := writableTestOptions(t, t.TempDir(), "-move-destination", "DEST="+destination)
	address, stop := startWritableQRSCP(t, opts)
	uid := seriesA + ".201"
	if status := networkStore(t, address, uid); status != dimse.StatusSuccess {
		t.Fatalf("store status%04X", status)
	}
	matches, status := find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA, "NumberOfStudyRelatedInstances": ""})
	if status != dimse.StatusSuccess || len(matches) != 1 || get(matches[0], "NumberOfStudyRelatedInstances") != "1" {
		t.Fatalf("FIND %04X %d", status, len(matches))
	}
	response := move(t, address, "DEST", map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA})
	if response.Status != dimse.StatusSuccess || len(moved.sorted()) != 1 || moved.sorted()[0] != uid {
		t.Fatalf("MOVE %+v %v", response, moved.sorted())
	}
	contexts := []ul.PresentationContext{{AbstractSyntaxUID: dimse.StudyRootGetSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}, {AbstractSyntaxUID: dimsetest.SecondaryCaptureSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}}
	assoc := dial(t, address, contexts, []ul.RoleSelectionItem{{SopClassUID: dimsetest.SecondaryCaptureSOPClassUID, SCPRole: true}})
	client, err := dimse.NewRetrieveClientForSOPClass(assoc, dimse.StudyRootGetSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored := 0
	getResponse, err := client.Get(ctx, dimse.CGetRequest{AffectedSOPClassUID: dimse.StudyRootGetSOPClassUID, MessageID: 1}, identifier(map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA}), dimse.CGetStoreHandlerFunc(func(_ context.Context, request dimse.CGetStoreRequestContext) (uint16, error) {
		if request.Request.AffectedSOPInstanceUID != uid {
			t.Error("wrong C-GET identity")
		}
		pixel, ok := request.DataSet.Get(core.NewTag(0x7FE0, 0x0010))
		raw, _ := pixel.RawBytes()
		if !ok || !bytes.Equal(raw, []byte{0, 64, 128, 255}) {
			t.Error("C-GET pixel bytes differ")
		}
		stored++
		return dimse.StatusSuccess, nil
	}))
	if err != nil || getResponse.Status != dimse.StatusSuccess || stored != 1 {
		t.Fatalf("GET %+v %v count%d", getResponse, err, stored)
	}
	assoc.Release(ctx)
	stop()
	address, _ = startWritableQRSCP(t, opts)
	matches, status = find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA})
	if status != dimse.StatusSuccess || len(matches) != 1 {
		t.Fatalf("restart FIND %04X %d", status, len(matches))
	}
	if status := networkStore(t, address, uid); status != dimse.StatusCStoreDataSetDoesNotMatch {
		t.Fatalf("restart duplicate%04X", status)
	}
}

func TestWritableConcurrentQueryStoreAndInjectedFind(t *testing.T) {
	opts := writableTestOptions(t, t.TempDir(), "-max-associations", "16")
	address, _ := startWritableQRSCP(t, opts)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			uid := seriesA + ".21" + string(rune('0'+i))
			if status := networkStore(t, address, uid); status != dimse.StatusSuccess {
				t.Errorf("store%04X", status)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, status := find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA})
			if status != dimse.StatusSuccess {
				t.Errorf("find%04X", status)
			}
		}()
	}
	wg.Wait()
	matches, status := find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": studyA, "NumberOfStudyRelatedInstances": ""})
	if status != dimse.StatusSuccess || len(matches) != 1 || get(matches[0], "NumberOfStudyRelatedInstances") != "4" {
		t.Fatal("publication snapshot omitted objects")
	}
	opts = writableTestOptions(t, t.TempDir(), "-cfind-error", "0xC123")
	address, _ = startWritableQRSCP(t, opts)
	matches, status = find(t, address, dimse.StudyRootFindSOPClassUID, map[string]string{"QueryRetrieveLevel": "STUDY", "StudyInstanceUID": ""})
	if status != 0xC123 || len(matches) != 0 {
		t.Fatalf("injected FIND %04X %d", status, len(matches))
	}
}
