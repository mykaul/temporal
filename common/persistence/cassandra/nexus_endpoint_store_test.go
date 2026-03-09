package cassandra

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyBatchCASRows_PartitionStatusFirst(t *testing.T) {
	row1 := map[string]any{"type": rowTypePartitionStatus, "version": int64(5)}
	row2 := map[string]any{"type": rowTypeNexusEndpoint, "id": "abc"}

	partitionStatus, endpoint, err := classifyBatchCASRows(row1, row2)
	require.NoError(t, err)
	require.Equal(t, row1, partitionStatus)
	require.Equal(t, row2, endpoint)
}

func TestClassifyBatchCASRows_EndpointFirst(t *testing.T) {
	row1 := map[string]any{"type": rowTypeNexusEndpoint, "id": "abc"}
	row2 := map[string]any{"type": rowTypePartitionStatus, "version": int64(5)}

	partitionStatus, endpoint, err := classifyBatchCASRows(row1, row2)
	require.NoError(t, err)
	require.Equal(t, row2, partitionStatus)
	require.Equal(t, row1, endpoint)
}

func TestClassifyBatchCASRows_NilRow2(t *testing.T) {
	row1 := map[string]any{"type": rowTypePartitionStatus, "version": int64(5)}

	partitionStatus, endpoint, err := classifyBatchCASRows(row1, nil)
	require.NoError(t, err)
	require.Equal(t, row1, partitionStatus)
	require.Nil(t, endpoint)
}

func TestClassifyBatchCASRows_UnexpectedType(t *testing.T) {
	row1 := map[string]any{"type": 99}
	row2 := map[string]any{"type": rowTypePartitionStatus}

	_, _, err := classifyBatchCASRows(row1, row2)
	require.ErrorContains(t, err, "unexpected row type 99")
}

func TestClassifyBatchCASRows_MissingTypeField(t *testing.T) {
	row1 := map[string]any{"version": int64(5)}
	row2 := map[string]any{"type": rowTypeNexusEndpoint}

	_, _, err := classifyBatchCASRows(row1, row2)
	require.ErrorContains(t, err, "error reading type from CAS result row")
}
