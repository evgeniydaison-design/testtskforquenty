package transport

import (
	"encoding/json"
	"testing"

	"board/internal/crdt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvelope_MarshalSnapshot_Roundtrip(t *testing.T) {
	c := NewJSONCodec()
	els := []crdt.Element{
		{ID: "a", Type: crdt.TypeRect, Props: map[crdt.PropName]*crdt.Register{
			crdt.PropX: {Value: 1.0, TS: crdt.HLC{Wall: 100, Node: "c1"}},
			crdt.PropY: {Value: 2.0, TS: crdt.HLC{Wall: 100, Node: "c1"}},
		}},
		{ID: "b", Type: crdt.TypeLine, Props: map[crdt.PropName]*crdt.Register{
			crdt.PropPoints: {Value: [][]float64{{0, 0}, {1, 1}}, TS: crdt.HLC{Wall: 200, Node: "c2"}},
		}},
	}
	b, err := c.MarshalSnapshot(123, els)
	require.NoError(t, err)

	var env Envelope
	require.NoError(t, json.Unmarshal(b, &env))
	assert.Equal(t, TypeSnapshot, env.Type)
	assert.Equal(t, ProtoV1, env.Proto)

	var got SnapshotPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.Equal(t, uint64(123), got.Seq)
	require.Len(t, got.Elements, 2)
	assert.Equal(t, "a", got.Elements[0].ID)
	assert.Equal(t, crdt.TypeLine, got.Elements[1].Type)
	// Points сериализуется как [][]float64 в JSON.
	r, ok := got.Elements[1].Props[crdt.PropPoints]
	require.True(t, ok)
	v, ok := r.Value.([]any)
	require.True(t, ok, "expected []any after JSON roundtrip, got %T", r.Value)
	assert.Len(t, v, 2)
}

func TestEnvelope_MarshalOps_Roundtrip(t *testing.T) {
	c := NewJSONCodec()
	ops := []crdt.Op{
		{Kind: crdt.OpUpsert, ID: "a", TS: crdt.HLC{Wall: 100, Node: "c1"},
			Type: crdt.TypeRect, Props: map[crdt.PropName]any{crdt.PropX: 1.5}},
		{Kind: crdt.OpDelete, ID: "b", TS: crdt.HLC{Wall: 200, Node: "c1"}},
	}
	b, err := c.MarshalOps(42, "c1", ops)
	require.NoError(t, err)

	env, err := decodeEnvelope(b)
	require.NoError(t, err)
	assert.Equal(t, TypeOps, env.Type)

	var got OpsBroadcastPayload
	require.NoError(t, decodePayload(env.Payload, &got))
	assert.Equal(t, uint64(42), got.Seq)
	assert.Equal(t, "c1", got.Origin)
	require.Len(t, got.Ops, 2)
	assert.Equal(t, crdt.OpDelete, got.Ops[1].Kind)
	assert.Equal(t, int64(100), got.Ops[0].TS.Wall)
	assert.Equal(t, "c1", got.Ops[0].TS.Node)
}

func TestEnvelope_MarshalAck_Roundtrip(t *testing.T) {
	c := NewJSONCodec()
	b, err := c.MarshalAck("batch-1", 99)
	require.NoError(t, err)
	env, err := decodeEnvelope(b)
	require.NoError(t, err)
	var got AckPayload
	require.NoError(t, decodePayload(env.Payload, &got))
	assert.Equal(t, "batch-1", got.BatchID)
	assert.Equal(t, uint64(99), got.Seq)
}

func TestEnvelope_MarshalError_Roundtrip(t *testing.T) {
	c := NewJSONCodec()
	b, err := c.MarshalError("bad_ops", "что-то пошло не так")
	require.NoError(t, err)
	env, err := decodeEnvelope(b)
	require.NoError(t, err)
	var got ErrorPayload
	require.NoError(t, decodePayload(env.Payload, &got))
	assert.Equal(t, "bad_ops", got.Code)
	assert.Equal(t, "что-то пошло не так", got.Message)
}

func TestDecodeEnvelope_BadJSON(t *testing.T) {
	_, err := decodeEnvelope([]byte("не json"))
	assert.Error(t, err)
}

func TestDecodePayload_BadJSON(t *testing.T) {
	err := decodePayload(json.RawMessage(`{"seq":"это не число"}`), &SnapshotPayload{})
	assert.Error(t, err)
}

// FuzzDecodeEnvelope — грубая проверка: декодер не паникует на мусоре.
func FuzzDecodeEnvelope(f *testing.F) {
	f.Add([]byte(`{"type":"hello","proto":"v1","payload":{"clientId":"x"}}`))
	f.Add([]byte(`{"type":"ops"}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(`{"type":"presence","payload":{"cursor":{"x":1,"y":2}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := decodeEnvelope(data)
		if err != nil {
			return
		}
		_ = decodePayload(env.Payload, &HelloPayload{})
		_ = decodePayload(env.Payload, &OpsPayload{})
		_ = decodePayload(env.Payload, &SnapshotPayload{})
		_ = decodePayload(env.Payload, &AckPayload{})
		_ = decodePayload(env.Payload, &ErrorPayload{})
		_ = decodePayload(env.Payload, &PresenceBroadcastPayload{})
	})
}
