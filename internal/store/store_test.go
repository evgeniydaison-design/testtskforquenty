package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"

	"board/internal/crdt"
	"board/internal/room"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func newStore(t *testing.T) *FileStore {
	t.Helper()
	fs, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = fs.CloseAll() })
	return fs
}

func sampleOp(id string, wall int64, x float64) crdt.Op {
	return crdt.Op{
		Kind: crdt.OpUpsert,
		ID:   id,
		TS:   crdt.HLC{Wall: wall, Logical: 0, Node: "alice"},
		Type: crdt.TypeRect,
		Props: map[crdt.PropName]any{
			crdt.PropX: x,
		},
	}
}

// --- basics ---

func TestNew_EmptyRootError(t *testing.T) {
	_, err := New("")
	assert.Error(t, err)
}

func TestNew_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	fs, err := New(dir)
	require.NoError(t, err)
	defer fs.CloseAll()
	require.DirExists(t, dir)
}

func TestList_Empty(t *testing.T) {
	fs := newStore(t)
	ids, err := fs.List()
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestList_IncludesCreatedRooms(t *testing.T) {
	fs := newStore(t)
	require.NoError(t, fs.Append("a", room.Record{Seq: 1, Ops: []crdt.Op{sampleOp("x", 1, 1)}}))
	require.NoError(t, fs.Append("b", room.Record{Seq: 1, Ops: []crdt.Op{sampleOp("y", 1, 1)}}))
	require.NoError(t, fs.Flush("a"))
	require.NoError(t, fs.Flush("b"))
	ids, err := fs.List()
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, ids, "List сортирован и не включает мусор")
}

// --- roundtrip ---

func TestAppendLoad_RoundtripNoSnapshot(t *testing.T) {
	fs := newStore(t)
	recs := []room.Record{
		{Seq: 1, Origin: "alice", Ops: []crdt.Op{sampleOp("a", 1000, 10)}},
		{Seq: 2, Origin: "bob", Ops: []crdt.Op{sampleOp("b", 2000, 20)}},
		{Seq: 3, Origin: "alice", Ops: []crdt.Op{sampleOp("a", 3000, 30)}},
	}
	for _, r := range recs {
		require.NoError(t, fs.Append("demo", r))
	}
	require.NoError(t, fs.Flush("demo"))

	loaded, err := fs.Load("demo")
	require.NoError(t, err)
	assert.Nil(t, loaded.Snapshot, "снапшота не было — Load вернёт только records")
	require.Len(t, loaded.Records, 3)
	assert.Equal(t, uint64(1), loaded.Records[0].Seq)
	assert.Equal(t, "alice", loaded.Records[0].Origin)
	assert.Equal(t, uint64(3), loaded.Records[2].Seq)
}

func TestSnapshotLoad_Roundtrip(t *testing.T) {
	fs := newStore(t)
	snap := room.Snapshot{
		Seq:     42,
		SavedAt: 12345,
		Elements: []crdt.Element{{
			ID:   "e1",
			Type: crdt.TypeRect,
			Props: map[crdt.PropName]*crdt.Register{
				crdt.PropX: {Value: 7.5, TS: crdt.HLC{Wall: 100, Node: "alice"}},
			},
		}},
	}
	require.NoError(t, fs.SaveSnapshot("demo", snap))

	loaded, err := fs.Load("demo")
	require.NoError(t, err)
	require.NotNil(t, loaded.Snapshot)
	assert.Equal(t, uint64(42), loaded.Snapshot.Seq)
	require.Len(t, loaded.Snapshot.Elements, 1)
	// После SaveSnapshot WAL должен быть trunc'нут (records с Seq<=42 выброшены).
	assert.Empty(t, loaded.Records)
}

func TestLoad_MergesSnapshotAndWALTail(t *testing.T) {
	fs := newStore(t)
	// Запишем 5 батчей, «снимок» сделаем на Seq=3, потом ещё два.
	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, fs.Append("demo", room.Record{Seq: i, Ops: []crdt.Op{sampleOp("x", int64(i)*1000, float64(i))}}))
	}
	require.NoError(t, fs.SaveSnapshot("demo", room.Snapshot{
		Seq:      3,
		Elements: []crdt.Element{}, // пустой, сам факт что был
		SavedAt:  1,
	}))
	for i := uint64(4); i <= 5; i++ {
		require.NoError(t, fs.Append("demo", room.Record{Seq: i, Ops: []crdt.Op{sampleOp("x", int64(i)*1000, float64(i))}}))
	}
	require.NoError(t, fs.Flush("demo"))

	loaded, err := fs.Load("demo")
	require.NoError(t, err)
	require.NotNil(t, loaded.Snapshot)
	assert.Equal(t, uint64(3), loaded.Snapshot.Seq)
	require.Len(t, loaded.Records, 2, "WAL-tail содержит ровно записи >3")
	assert.Equal(t, uint64(4), loaded.Records[0].Seq)
	assert.Equal(t, uint64(5), loaded.Records[1].Seq)
}

func TestAppend_IgnoreDuplicateSeq(t *testing.T) {
	fs := newStore(t)
	require.NoError(t, fs.Append("demo", room.Record{Seq: 5, Ops: []crdt.Op{sampleOp("a", 5, 1)}}))
	require.NoError(t, fs.Flush("demo"))
	// Второй append с Seq<=5 — retry, должен тихо проигнорироваться.
	require.NoError(t, fs.Append("demo", room.Record{Seq: 5, Ops: []crdt.Op{sampleOp("dup", 5, 9)}}))
	require.NoError(t, fs.Append("demo", room.Record{Seq: 3, Ops: []crdt.Op{sampleOp("old", 3, 9)}}))
	require.NoError(t, fs.Flush("demo"))

	loaded, err := fs.Load("demo")
	require.NoError(t, err)
	require.Len(t, loaded.Records, 1, "дубликаты не должны попадать в WAL")
	assert.Equal(t, uint64(5), loaded.Records[0].Seq)
}

func TestLoad_PartialLastLineIsIgnored(t *testing.T) {
	fs := newStore(t)
	require.NoError(t, fs.Append("demo", room.Record{Seq: 1, Ops: []crdt.Op{sampleOp("a", 1, 1)}}))
	require.NoError(t, fs.Flush("demo"))
	// Имитируем «краш в середине записи»: добавим битую строку в конец
	// файла в обход writer'а.
	key := hexID("demo")
	walPath := filepath.Join(fs.root, key, "wal.log")
	f, err := os.OpenFile(walPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"seq":2,"ops":[`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	loaded, err := fs.Load("demo")
	require.NoError(t, err, "битый хвост — не фатальная ошибка")
	require.Len(t, loaded.Records, 1, "битая запись не должна попасть в результат")
	assert.Equal(t, uint64(1), loaded.Records[0].Seq)
}

func TestHexIDPathTraversalSafe(t *testing.T) {
	fs := newStore(t)
	// Даже если roomID содержит «../», hex-кодирование «съест» слэши,
	// и путь останется внутри root.
	require.NoError(t, fs.Append("../../etc/passwd", room.Record{Seq: 1, Ops: []crdt.Op{sampleOp("x", 1, 1)}}))
	require.NoError(t, fs.Flush("../../etc/passwd"))
	entries, err := os.ReadDir(fs.root)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "никаких файлов за пределами root")
	// И обратное чтение работает.
	loaded, err := fs.Load("../../etc/passwd")
	require.NoError(t, err)
	require.Len(t, loaded.Records, 1)
}

func TestSaveSnapshot_AtomicRenameLeavesNoTmp(t *testing.T) {
	fs := newStore(t)
	require.NoError(t, fs.SaveSnapshot("demo", room.Snapshot{Seq: 1, Elements: nil, SavedAt: 1}))
	key := hexID("demo")
	entries, err := os.ReadDir(filepath.Join(fs.root, key))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotEqual(t, "snapshot.json.tmp", e.Name(),
			"tmp-файл не должен оставаться после успешного save")
	}
}

// --- Close ---

func TestClose_FlushesAndReleases(t *testing.T) {
	fs := newStore(t)
	require.NoError(t, fs.Append("demo", room.Record{Seq: 1, Ops: []crdt.Op{sampleOp("x", 1, 1)}}))
	require.NoError(t, fs.Close("demo"))
	// После close writer'а нет — следующий append откроет заново.
	fs.mu.Lock()
	_, ok := fs.writers[hexID("demo")]
	fs.mu.Unlock()
	assert.False(t, ok)

	// Данные всё равно на диске.
	loaded, err := fs.Load("demo")
	require.NoError(t, err)
	require.Len(t, loaded.Records, 1)
}

// --- helpers ---

func TestRecordJSONShape(t *testing.T) {
	// Гарантируем: поле «ops» — массив Op-объектов с expected json-ключами.
	// Это внешний контракт: менять — значит ломать клиентов.
	r := room.Record{Seq: 7, Origin: "a", Ops: []crdt.Op{sampleOp("x", 100, 1)}}
	b, err := json.Marshal(r)
	require.NoError(t, err)
	var generic map[string]any
	require.NoError(t, json.Unmarshal(b, &generic))
	assert.EqualValues(t, 7, generic["seq"])
	assert.Equal(t, "a", generic["origin"])
	require.IsType(t, []any{}, generic["ops"])
	ops := generic["ops"].([]any)
	require.Len(t, ops, 1)
	op0 := ops[0].(map[string]any)
	assert.Equal(t, "upsert", op0["kind"])
	assert.Equal(t, "x", op0["id"])
	// HLC «w/l/n» — то же имя, что на wire.
	ts := op0["ts"].(map[string]any)
	assert.EqualValues(t, 100, ts["w"])
}
