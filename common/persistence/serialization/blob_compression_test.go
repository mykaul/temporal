package serialization

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
)

func TestIsCompressedEncoding(t *testing.T) {
	require.True(t, IsCompressedEncoding("proto3+zstd"))
	require.True(t, IsCompressedEncoding("Proto3+Zstd"))
	require.False(t, IsCompressedEncoding("proto3"))
	require.False(t, IsCompressedEncoding("json"))
	require.False(t, IsCompressedEncoding(""))
}

func TestCompressDecompressRoundTrip(t *testing.T) {
	// Create a blob large enough that compression is meaningful.
	original := bytes.Repeat([]byte("hello world event data "), 1000)
	blob := &commonpb.DataBlob{
		Data:         original,
		EncodingType: enumspb.ENCODING_TYPE_PROTO3,
	}

	compressed, encodingStr, err := CompressHistoryEventBlob(blob)
	require.NoError(t, err)
	require.Equal(t, EncodingTypeProto3Zstd, encodingStr)
	// Compressed should be smaller.
	require.Less(t, len(compressed.Data), len(original))
	require.Equal(t, enumspb.ENCODING_TYPE_PROTO3, compressed.EncodingType)

	// Decompress.
	decompressed, err := DecompressHistoryEventBlob(compressed.Data, encodingStr)
	require.NoError(t, err)
	require.Equal(t, enumspb.ENCODING_TYPE_PROTO3, decompressed.EncodingType)
	require.Equal(t, original, decompressed.Data)
}

func TestDecompressHistoryEventBlob_NotCompressed(t *testing.T) {
	data := []byte("raw proto data")
	blob, err := DecompressHistoryEventBlob(data, enumspb.ENCODING_TYPE_PROTO3.String())
	require.NoError(t, err)
	require.Equal(t, enumspb.ENCODING_TYPE_PROTO3, blob.EncodingType)
	require.Equal(t, data, blob.Data)
}

func TestCompressHistoryEventBlob_NilBlob(t *testing.T) {
	blob, encodingStr, err := CompressHistoryEventBlob(nil)
	require.NoError(t, err)
	require.Nil(t, blob)
	require.Empty(t, encodingStr)
}

func TestCompressHistoryEventBlob_EmptyData(t *testing.T) {
	blob := &commonpb.DataBlob{
		Data:         nil,
		EncodingType: enumspb.ENCODING_TYPE_PROTO3,
	}
	result, encodingStr, err := CompressHistoryEventBlob(blob)
	require.NoError(t, err)
	require.Same(t, blob, result)
	require.Empty(t, encodingStr)
}

func TestDecompressHistoryEventBlob_CorruptedData(t *testing.T) {
	_, err := DecompressHistoryEventBlob([]byte("not valid zstd"), EncodingTypeProto3Zstd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to decompress")
}

func TestCompressHistoryEventBlob_IncompressibleData(t *testing.T) {
	// Use already-compressed data to guarantee incompressibility:
	// zstd output has maximum entropy and cannot be compressed further.
	randomSource := bytes.Repeat([]byte("ab"), 1024)
	preCompressed := zstdEncoder.EncodeAll(randomSource, nil)

	blob := &commonpb.DataBlob{
		Data:         preCompressed,
		EncodingType: enumspb.ENCODING_TYPE_PROTO3,
	}

	result, encodingStr, err := CompressHistoryEventBlob(blob)
	require.NoError(t, err)
	// Compressing already-compressed data should not help — returns original.
	require.Same(t, blob, result)
	require.Empty(t, encodingStr)
}
