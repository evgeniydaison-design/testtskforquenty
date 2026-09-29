package crdt

import (
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helpers — короткое конструирование ops.
func up(id string, ts HLC, x float64) Op {
	return Op{
		Kind:  OpUpsert,
		ID:    id,
		TS:    ts,
		Type:  TypeRect,
		Props: map[PropName]any{PropX: x, PropW: 10.0, PropH: 10.0},
	}
}

func del(id string, ts HLC) Op {
	return Op{Kind: OpDelete, ID: id, TS: ts}
}

func hlc(wall int64, logical uint32, node string) HLC {
	return HLC{Wall: wall, Logical: logical, Node: node}
}

// --- базовое слияние ---

func TestDoc_Apply_NewElement(t *testing.T) {
	d := NewDoc()
	require.True(t, d.Apply(up("a", hlc(100, 0, "c1"), 10)))

	el, ok := d.Get("a")
	require.True(t, ok)
	v, ok := el.Get(PropX)
	require.True(t, ok)
	assert.Equal(t, 10.0, v)
	assert.Equal(t, TypeRect, el.Type)
}

func TestDoc_Apply_HigherTSHLCWins(t *testing.T) {
	d := NewDoc()
	require.True(t, d.Apply(up("a", hlc(100, 0, "c1"), 10)))
	require.True(t, d.Apply(up("a", hlc(200, 0, "c1"), 20)))
	el, _ := d.Get("a")
	v, _ := el.Get(PropX)
	assert.Equal(t, 20.0, v)
}

func TestDoc_Apply_LowerTSIgnored(t *testing.T) {
	d := NewDoc()
	require.True(t, d.Apply(up("a", hlc(200, 0, "c1"), 100)))
	require.False(t, d.Apply(up("a", hlc(100, 0, "c1"), 1)),
		"старый оп не должен победить")
	el, _ := d.Get("a")
	v, _ := el.Get(PropX)
	assert.Equal(t, 100.0, v)
}

// Это — ключевой сценарий, ради которого делался Этап 3: «A двигает —
// Б красит». Lamport-LWW на весь элемент терял одно изменение. Per-property
// LWW сохраняет ОБА.
func TestDoc_Apply_ConcurrentProps_BothSurvive(t *testing.T) {
	d := NewDoc()
	// Сначала создадим элемент (нужен Type).
	require.True(t, d.Apply(up("a", hlc(100, 0, "c1"), 0)))

	// A двигает: меняет X/Y.
	opMove := Op{
		Kind: OpUpsert, ID: "a", TS: hlc(200, 0, "alice"),
		Props: map[PropName]any{PropX: 50.0, PropY: 60.0},
	}
	// B красит: меняет Stroke.
	opColor := Op{
		Kind: OpUpsert, ID: "a", TS: hlc(200, 1, "bob"),
		Props: map[PropName]any{PropStroke: "#ff0000"},
	}
	// Применим в любом порядке — итог одинаковый.
	require.True(t, d.Apply(opMove))
	require.True(t, d.Apply(opColor))

	el, _ := d.Get("a")
	x, _ := el.Get(PropX)
	y, _ := el.Get(PropY)
	s, _ := el.Get(PropStroke)
	assert.Equal(t, 50.0, x, "движение A сохранено")
	assert.Equal(t, 60.0, y, "движение A сохранено")
	assert.Equal(t, "#ff0000", s, "цвет B сохранён")
}

// То же, но в обратном порядке применения — результат идентичен
// (commutativity).
func TestDoc_Apply_ConcurrentProps_Commutative(t *testing.T) {
	d := NewDoc()
	require.True(t, d.Apply(up("a", hlc(100, 0, "c1"), 0)))
	opMove := Op{
		Kind: OpUpsert, ID: "a", TS: hlc(200, 0, "alice"),
		Props: map[PropName]any{PropX: 50.0},
	}
	opColor := Op{
		Kind: OpUpsert, ID: "a", TS: hlc(200, 1, "bob"),
		Props: map[PropName]any{PropStroke: "#ff0000"},
	}
	// Обратный порядок.
	require.True(t, d.Apply(opColor))
	require.True(t, d.Apply(opMove))

	el, _ := d.Get("a")
	x, _ := el.Get(PropX)
	s, _ := el.Get(PropStroke)
	assert.Equal(t, 50.0, x)
	assert.Equal(t, "#ff0000", s)
}

// Одинаковый (Wall, Logical), разные Node — больший Node побеждает.
func TestDoc_Apply_TieBreakByNode(t *testing.T) {
	d := NewDoc()
	require.True(t, d.Apply(up("a", hlc(100, 0, "bob"), 1)))
	// тот же Wall+Logical, но alice < bob → alice проигрывает.
	require.False(t, d.Apply(up("a", hlc(100, 0, "alice"), 2)))
	el, _ := d.Get("a")
	v, _ := el.Get(PropX)
	assert.Equal(t, 1.0, v, "bob остаётся победителем")
}

func TestDoc_Apply_Idempotent(t *testing.T) {
	d := NewDoc()
	op := up("a", hlc(100, 0, "c1"), 42)
	require.True(t, d.Apply(op))
	require.False(t, d.Apply(op),
		"повтор того же оп-а — состояние не меняется")
}

// --- delete / resurrect ---

func TestDoc_Apply_DeleteKeepsTombstone(t *testing.T) {
	d := NewDoc()
	require.True(t, d.Apply(up("a", hlc(100, 0, "c1"), 10)))
	require.True(t, d.Apply(del("a", hlc(200, 0, "c1"))))

	el, ok := d.Get("a")
	require.True(t, ok, "tombstone обязан остаться в карте")
	assert.True(t, el.Deleted())

	// Late upsert с меньшим TS не должен воскресить элемент.
	require.False(t, d.Apply(up("a", hlc(50, 0, "c2"), 99)))
	el, _ = d.Get("a")
	assert.True(t, el.Deleted())

	// Upsert с бо́льшим TS — «undelete».
	require.True(t, d.Apply(up("a", hlc(300, 0, "c2"), 99)))
	el, _ = d.Get("a")
	assert.False(t, el.Deleted(), "delete должен быть «отменён» новым upsert")
}

func TestDoc_Apply_DeleteOnUnknownID(t *testing.T) {
	d := NewDoc()
	// Delete «в пустоту» кладёт tombstone, чтобы позже пришедший upsert
	// с меньшим TS не создал элемент.
	require.True(t, d.Apply(del("z", hlc(100, 0, "c1"))))
	el, ok := d.Get("z")
	require.True(t, ok)
	assert.True(t, el.Deleted())
	// Upsert с меньшим TS проиграл.
	require.False(t, d.Apply(up("z", hlc(50, 0, "c2"), 1)))
}

// --- snapshot / rank order ---

func TestDoc_Snapshot_HidesTombstones(t *testing.T) {
	d := NewDoc()
	// rank задаём вручную.
	opA := up("a", hlc(100, 0, "c1"), 10)
	opA.Props[PropRank] = "b"
	opB := up("b", hlc(100, 0, "c1"), 20)
	opB.Props[PropRank] = "d"
	require.True(t, d.Apply(opA))
	require.True(t, d.Apply(opB))
	require.True(t, d.Apply(del("b", hlc(200, 0, "c1"))))

	snap := d.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "a", snap[0].ID)

	all := d.All()
	assert.Len(t, all, 2, "All() включает tombstones")
}

// Порядок Snapshot — по rank лексикографически.
func TestDoc_Snapshot_SortedByRank(t *testing.T) {
	d := NewDoc()
	// Вставляем в «разброс», проверка что сортировка по rank.
	for _, pair := range []struct {
		id   string
		rank string
	}{{"a", "z"}, {"b", "m"}, {"c", "5"}} {
		op := Op{
			Kind: OpUpsert, ID: pair.id, TS: hlc(100, 0, "c1"),
			Type: TypeRect, Props: map[PropName]any{PropRank: pair.rank},
		}
		require.True(t, d.Apply(op))
	}
	snap := d.Snapshot()
	require.Len(t, snap, 3)
	assert.Equal(t, "c", snap[0].ID, "rank=5")
	assert.Equal(t, "b", snap[1].ID, "rank=m")
	assert.Equal(t, "a", snap[2].ID, "rank=z")
}

// --- reset / bulk load ---

func TestDoc_Reset(t *testing.T) {
	d := NewDoc()
	require.True(t, d.Apply(up("a", hlc(100, 0, "c1"), 10)))
	// Загружаем «с нуля»: два элемента.
	d.Reset([]Element{
		{ID: "z", Type: TypeRect, Props: map[PropName]*Register{
			PropX:  {Value: 5.0, TS: hlc(500, 0, "c2")},
			PropRank: {Value: "a", TS: hlc(500, 0, "c2")},
		}},
	})
	require.Equal(t, 1, d.Len())
	_, ok := d.Get("a")
	assert.False(t, ok)
	el, ok := d.Get("z")
	require.True(t, ok)
	v, _ := el.Get(PropX)
	assert.Equal(t, 5.0, v)
}

// --- convergence property test ---

// Doc обязан сходиться при любом порядке применения одного и того же
// множества ops. Это commutativity + idempotence.
func TestDoc_ConvergenceProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	// Генерируем «хаотичный» набор ops от 3 клиентов над 20 элементами.
	// Разные свойства, пересекающиеся таймстемпы.
	var ops []Op
	for i := 0; i < 200; i++ {
		node := []string{"alice", "bob", "carol"}[rng.Intn(3)]
		id := "e" + strconv.Itoa(rng.Intn(20))
		ts := hlc(int64(rng.Intn(1000)), uint32(rng.Intn(10)), node)
		op := Op{
			Kind: OpUpsert, ID: id, TS: ts, Type: TypeRect,
			Props: map[PropName]any{},
		}
		if rng.Intn(2) == 0 {
			op.Props[PropX] = float64(rng.Intn(1000))
		}
		if rng.Intn(2) == 0 {
			op.Props[PropY] = float64(rng.Intn(1000))
		}
		if rng.Intn(3) == 0 {
			op.Props[PropStroke] = "#" + strconv.Itoa(rng.Intn(0xffffff))
		}
		ops = append(ops, op)
	}

	// «Эталонный» порядок.
	dA := NewDoc()
	for _, op := range ops {
		dA.Apply(op)
	}

	// Случайная перестановка.
	shuffled := make([]Op, len(ops))
	copy(shuffled, ops)
	rng.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	dB := NewDoc()
	for _, op := range shuffled {
		dB.Apply(op)
	}

	assert.ElementsMatch(t, dA.All(), dB.All())
}

// --- GC ---

func TestDoc_SweepTombstones(t *testing.T) {
	d := NewDoc()
	// Старый delete (Wall=1000 мс — 1970 год).
	require.True(t, d.Apply(del("old", hlc(1000, 0, "c1"))))
	// Свежий delete (сейчас).
	require.True(t, d.Apply(del("new", hlc(time.Now().UnixMilli(), 0, "c1"))))
	require.Equal(t, 2, d.Len())

	// Cutoff: 30 дней назад.
	n := d.SweepTombstones(time.Now().Add(-DefaultTombstoneRetention))
	assert.Equal(t, 1, n)
	_, ok := d.Get("old")
	assert.False(t, ok, "старый tombstone удалён")
	_, ok = d.Get("new")
	assert.True(t, ok, "свежий tombstone остался")
}

func TestDoc_SweepTombstones_IgnoresLive(t *testing.T) {
	d := NewDoc()
	require.True(t, d.Apply(up("live", hlc(1000, 0, "c1"), 1)))
	n := d.SweepTombstones(time.Now().Add(-DefaultTombstoneRetention))
	assert.Equal(t, 0, n)
	_, ok := d.Get("live")
	assert.True(t, ok)
}

// --- benchmarks ---

func BenchmarkDocApply(b *testing.B) {
	d := NewDoc()
	ops := make([]Op, 1000)
	for i := range ops {
		ops[i] = Op{
			Kind: OpUpsert, ID: "e" + itoa(i%500), TS: hlc(int64(i), 0, "c1"),
			Type:  TypeRect,
			Props: map[PropName]any{PropX: float64(i)},
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Apply(ops[i%len(ops)])
	}
}

func BenchmarkDocConcurrentProps(b *testing.B) {
	// Сценарий: два клиента параллельно правят РАЗНЫЕ свойства одного
	// элемента. Именно это даёт per-property LWW.
	d := NewDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Apply(Op{Kind: OpUpsert, ID: "a", TS: hlc(int64(i*2), 0, "alice"),
			Props: map[PropName]any{PropX: float64(i)}})
		d.Apply(Op{Kind: OpUpsert, ID: "a", TS: hlc(int64(i*2+1), 0, "bob"),
			Props: map[PropName]any{PropStroke: "#fff"}})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
