package jpeglossless

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// These hashes freeze the encoder bytes from 212dd7fb before the performance
// change. They cover all predictors, signed samples, colour and category 16.
var losslessEncoderByteGoldens = map[string]string{
	"sv1-profile0": "d8fd70a62cdff7df248ab576aad60825b230e181b36d0d0b3851556464e888bd",
	"sv1-profile1": "9f98e62075c7513fb04d7b77dff6cb5bbb22c4976d518bf39c6c84108e7bda9f",
	"sv1-profile2": "1d4572d7ca2a78edb796db1b3caa617f7160d335cf2f48da412efcde37bf1ef7",
	"sv1-profile3": "ab796ac57a6c3a1764616edb527a5ec78cf9a44772511c0a8947fdf18788cfff",
	"sv1-profile4": "ab796ac57a6c3a1764616edb527a5ec78cf9a44772511c0a8947fdf18788cfff",
	"sv1-profile5": "cde317eab3458c877a1fcd0818b2022039fb2856dce3fc89033e1eece57b1767",
	"sv1-profile6": "cde317eab3458c877a1fcd0818b2022039fb2856dce3fc89033e1eece57b1767",
	"sv1-profile7": "23707c500053c0452fc859227df16ef7e677455d416fc5a8007f333711d3dd8f",
	"sv1-profile8": "cd80415b79aecf8a82124fcdf0dec4ecc4a00b6b930d7852b9914283f1f91a1f",
	"sv1-profile9": "b5b7704f5706071568ee57b4bac714dc245255402ca604e5cf41f78d4cb68983",
	"sv2-profile0": "e7f76b0c4dd63e0a35249918a319e88ca83cf9f11bfd8bb006f34df05f1ce9a5",
	"sv2-profile1": "d87432172ac814440997adc9b7bf06c541f2fd6537c25bdc9aa996ce2a33b1d6",
	"sv2-profile2": "8d8eed9b7139b2a4af39c92c68ab9ec96466487fdeaca95f04f0678c6b1e8d8b",
	"sv2-profile3": "cb31c1a89f769e249b7b99dc51dfdeed4104844ada4ddd1564367a4a0e3e1da2",
	"sv2-profile4": "cb31c1a89f769e249b7b99dc51dfdeed4104844ada4ddd1564367a4a0e3e1da2",
	"sv2-profile5": "5e5c8146a58f7bfa6a719e6086dd0bbc8bb5a1ab71f1f7bc4dbaf61d67b35274",
	"sv2-profile6": "5e5c8146a58f7bfa6a719e6086dd0bbc8bb5a1ab71f1f7bc4dbaf61d67b35274",
	"sv2-profile7": "c1bd2503307dc62726767decf5077292abcfe1f2edccb7ec905e70aef5a23532",
	"sv2-profile8": "a6b9b09e4a608f6bc44271a87449388a6ae406660af5fd2f01bb009b65fb19ff",
	"sv2-profile9": "605125ecc2a61a5f659fdb5adfa7788833318ec1be21ed4e7963a8ab99369dc9",
	"sv3-profile0": "31b9a1c5ddc281515351f6fa72e2253b1e2b836aea003380fdfc945509211ff4",
	"sv3-profile1": "41fff656132c17a10fa23476ffe41973f6a545e372fd6ca5ee601654e13b2a8f",
	"sv3-profile2": "5d8f840893c7ddd88bb9909abde6ac090c0449bfd582e99bddb55a2c1919eba9",
	"sv3-profile3": "bb4be7a448b5a2553b5992139b769f3cc368ed79520d7e8c2506b9ce7d703ffb",
	"sv3-profile4": "bb4be7a448b5a2553b5992139b769f3cc368ed79520d7e8c2506b9ce7d703ffb",
	"sv3-profile5": "4c49fd57bee2aeeb163a1c620e098ad5070834b66f86ed4c62a5823d7d20fffb",
	"sv3-profile6": "4c49fd57bee2aeeb163a1c620e098ad5070834b66f86ed4c62a5823d7d20fffb",
	"sv3-profile7": "a651a83abef013900ade5fe72ffc87addc88de1f3aa9b35b9ea62ca7aab51342",
	"sv3-profile8": "2727085fe0dd5825d78e2c2659bf704fafed99f61f8d2209b5247d0b84922d53",
	"sv3-profile9": "88682a1e4ce35af9043d6cf42624eb0c865f1e5033f0da6dae5e06a8fd5c1a1a",
	"sv4-profile0": "4c0c43d600012c2ead29c66304fde75896759801989a22e8cb7091323b81069e",
	"sv4-profile1": "8a1d3eeb966838ed99e3551e96365918eef25621b8cecb574acde3033fa97f73",
	"sv4-profile2": "31ea31a14a0a2cb619455af9746eaa99b340bff1cf793ebedfbfb48baa510530",
	"sv4-profile3": "716746a7de7b5846b18d0ed8a628a7f61664f6eef57698e6b02575ae4c37568f",
	"sv4-profile4": "716746a7de7b5846b18d0ed8a628a7f61664f6eef57698e6b02575ae4c37568f",
	"sv4-profile5": "9b2adf2e666a174dc0998be3ecaef3b6055b2a7f3fe70496504d0c17a17d3174",
	"sv4-profile6": "9b2adf2e666a174dc0998be3ecaef3b6055b2a7f3fe70496504d0c17a17d3174",
	"sv4-profile7": "b64c65a45101494650a35c2e4b67d3c73c0789ef61b98dab41b12162ea493f3e",
	"sv4-profile8": "ee2ecb72a58fe6652cecf614c295cd5e7182379d6b7bd8bec4e200dd2a852235",
	"sv4-profile9": "ccb50c48dcc70bf1e28f043dc36a46d961bd6f94ceacc034c74496b14109e63a",
	"sv5-profile0": "a7e60bf8b358309fc6241484224d7db0132fa2b67e54c2b5a59f567682a1a86e",
	"sv5-profile1": "54f4d99b7ed392a880d5cd088c9f84a8df5bba9799deb094a9d1451bae153d81",
	"sv5-profile2": "9a2ce80d88cdb70e07b5c4d15496e9ecd6291b4b4c6356d87d46341b2a510ed0",
	"sv5-profile3": "dfc1951dcacdb6f53235e4f8c234c0db2137361b5b9a87721df24084244c401e",
	"sv5-profile4": "dfc1951dcacdb6f53235e4f8c234c0db2137361b5b9a87721df24084244c401e",
	"sv5-profile5": "ce5dfa525610279f224c09ce4c49a1de6484ba95a382e95be45ac1e3b93b3bb8",
	"sv5-profile6": "ce5dfa525610279f224c09ce4c49a1de6484ba95a382e95be45ac1e3b93b3bb8",
	"sv5-profile7": "b71cf8968ab96bd6cbb1669009c125098efd24e6fafa821b778075d5b879daf9",
	"sv5-profile8": "c0c5052d915c80752604a1448d473a161436380f22522c1fcf30f30c3c43a4de",
	"sv5-profile9": "ebc8dcb52de0e9c0ec39a33e84491fde5bab04f70d553f1835442ff6883e4bdc",
	"sv6-profile0": "4e30b54127018ee42e2f3b6ebf6307805ddf636a46e243f7c9a3178cdcf3c8e6",
	"sv6-profile1": "ba11586214ac6f64a5a3b54c3880650daca1bec4d622f1df0a24d6d36dc917e3",
	"sv6-profile2": "9a46beb882ec6e94185aea9709d8e4a242bb0f6a08bcbd8912bce106c2a168dc",
	"sv6-profile3": "5bae8970242a3d83d053ff8adc8744cf96912b28837540526ccd4291dc893473",
	"sv6-profile4": "5bae8970242a3d83d053ff8adc8744cf96912b28837540526ccd4291dc893473",
	"sv6-profile5": "41bbe9c1dd4a90f5eb7cfbd9bcc969b116cd4824beed44b39998942505a3bd0c",
	"sv6-profile6": "41bbe9c1dd4a90f5eb7cfbd9bcc969b116cd4824beed44b39998942505a3bd0c",
	"sv6-profile7": "137fe986e1a55d9b615e40cff08946c3e3a819905fdc7c04fb5e88d4a3578251",
	"sv6-profile8": "7643b30ff25fc59e941e2776d4b1bac4abda6ade1f32c60dc73d7c03f1d7c287",
	"sv6-profile9": "18dd26d7c7052a3278d6b6a7aa61b087f13fd825f62a9cf48853fc680f41aa25",
	"sv7-profile0": "b242068411f2087d1c848b5fdcb2e48e8397a51693ea18e69ee34ebc6c3f9eab",
	"sv7-profile1": "ec769670fd4eea0d1b3b5f53e1a5332b26ea4c69cc7a4be15161ba6e2693fa42",
	"sv7-profile2": "58d75685bdcc68a4534015945023e8bea3b6be2d6a80b920a9d34c973b9ba0e7",
	"sv7-profile3": "5ba8cdd66cd5584c9982b8da61c9218310343e22c887b1955e925f8fba376102",
	"sv7-profile4": "5ba8cdd66cd5584c9982b8da61c9218310343e22c887b1955e925f8fba376102",
	"sv7-profile5": "fec503f032553b9486d5546ad3d5c7e7e80ad3214c5ca8bbc75bae00fa83459b",
	"sv7-profile6": "fec503f032553b9486d5546ad3d5c7e7e80ad3214c5ca8bbc75bae00fa83459b",
	"sv7-profile7": "93298f0a8656e2d58f141af55ab604085fa11149008f28a27baddb16123e0608",
	"sv7-profile8": "ea3cf025cefc9f735849daca97bcff741a18eeab784041eff5ff4910d36c6c40",
	"sv7-profile9": "f057f5c62da04b9163f3a6e17ba2c5b74d28be61b8b1bf0e52e493b6a3d5f691",
}

func losslessPerformanceProfiles() []pixeldata.Metadata {
	return []pixeldata.Metadata{
		encoderTestMetadata(13, 29, 1, 8, 2, 0, "MONOCHROME2"),
		encoderTestMetadata(13, 29, 1, 8, 8, 0, "MONOCHROME1"),
		encoderTestMetadata(13, 29, 1, 8, 7, 1, "MONOCHROME2"),
		encoderTestMetadata(13, 29, 1, 16, 12, 0, "MONOCHROME2"),
		encoderTestMetadata(13, 29, 1, 16, 12, 1, "MONOCHROME2"),
		encoderTestMetadata(13, 29, 1, 16, 16, 0, "MONOCHROME2"),
		encoderTestMetadata(13, 29, 1, 16, 16, 1, "MONOCHROME2"),
		encoderTestMetadata(13, 29, 3, 8, 8, 0, "RGB"),
		encoderTestMetadata(13, 29, 3, 16, 12, 0, "RGB"),
		encoderTestMetadata(13, 29, 3, 16, 16, 0, "YBR_FULL"),
	}
}

func TestLosslessEncoderPreservesPreOptimizationBytes(t *testing.T) {
	if len(losslessEncoderByteGoldens) != 70 {
		t.Fatal("pre-optimization corpus must contain all 70 bitstreams")
	}
	for predictor := 1; predictor <= 7; predictor++ {
		encoder, err := NewProcess14Encoder(predictor)
		if err != nil {
			t.Fatal(err)
		}
		for index, metadata := range losslessPerformanceProfiles() {
			name := fmt.Sprintf("sv%d-profile%d", predictor, index)
			frame := encoderTestFrame(metadata, predictor+int(metadata.BitsStored))
			encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprintf("%x", sha256.Sum256(encoded.Data))
			want, exists := losslessEncoderByteGoldens[name]
			if !exists {
				t.Errorf("missing baseline hash for %s", name)
				continue
			}
			if got != want {
				t.Errorf("%s changed bitstream: got %s want %s", name, got, want)
			}
		}
	}
}

func benchmarkLosslessDecode(b *testing.B, metadata pixeldata.Metadata) {
	frame := smoothTestFrame(metadata)
	encoded, err := NewSV1Encoder().EncodeFrame(context.Background(), frame, metadata)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		decoded, _, err := decodeFrameWithRecovery(context.Background(), encoded.Data, 1, metadata, encoderQuirks{}, false)
		if err != nil {
			b.Fatal(err)
		}
		if output := decoded.toInterleavedBytes(metadata.BitsAllocated); len(output) != len(frame) {
			b.Fatal("decoded length changed")
		}
	}
}

func BenchmarkDecodeSV1MR16(b *testing.B) {
	benchmarkLosslessDecode(b, encoderTestMetadata(512, 512, 1, 16, 12, 0, "MONOCHROME2"))
}

func BenchmarkDecodeSV1RGB8(b *testing.B) {
	benchmarkLosslessDecode(b, encoderTestMetadata(480, 640, 3, 8, 8, 0, "RGB"))
}
