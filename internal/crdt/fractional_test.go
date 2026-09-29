package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBetween_EmptyBoth(t *testing.T) {
	r, err := Between("", "")
	require.NoError(t, err)
	assert.Equal(t, "i", r)
}

func TestBetween_Prepend(t *testing.T) {
	r, err := Between("", "i")
	require.NoError(t, err)
	assert.Less(t, r, "i")
}

func TestBetween_Append(t *testing.T) {
	r, err := Between("i", "")
	require.NoError(t, err)
	assert.Greater(t, r, "i")
}

// Midpoint в зазоре: a и b различаются больше, чем на 1.
func TestBetween_Midpoint(t *testing.T) {
	r, err := Between("b", "d")
	require.NoError(t, err)
	assert.Equal(t, "c", r)
}

// Соседние символы — «проваливаемся» на следующий уровень.
func TestBetween_AdjacentChars(t *testing.T) {
	r, err := Between("b", "c")
	require.NoError(t, err)
	assert.Greater(t, r, "b")
	assert.Less(t, r, "c")
}

// Многократная вставка в тот же интервал остаётся упорядоченной.
// Ограничение: base-36 даёт ~5 бит на уровень, поэтому после
// log2(36^maxRankLen) ≈ 10–12 сужений интервал исчерпывается — это
// нормально и обрабатывается вызывающим кодом как «пора на rebalance».
func TestBetween_RepeatedInsertion(t *testing.T) {
	low := "a"
	high := "z"
	var last string
	iters := 0
	for i := 0; i < 20; i++ {
		mid, err := Between(low, high)
		if err != nil {
			// Ожидаемое истощение где-то после 8–12 итераций.
			require.ErrorIs(t, err, ErrRankExhausted, "iter %d", i)
			break
		}
		assert.Greater(t, mid, low, "iter %d", i)
		assert.Less(t, mid, high, "iter %d", i)
		// Сужаем интервал: вставляем в первую половину.
		high = mid
		last = mid
		iters++
	}
	assert.GreaterOrEqual(t, iters, 5, "минимум 5 упорядоченных вставок")
	assert.NotEmpty(t, last)
}

// Adversarial: вставка «после только что вставленного» до упора.
// Ограничение maxRankLen должно дать ошибку, а не «бесконечный» ранг.
func TestBetween_ExhaustsEventually(t *testing.T) {
	// Начинаем с интервала [a, b], но каждый раз вставляем в левую
	// половину — ранги растут.
	low, high := "a", "b"
	for i := 0; i < 30; i++ {
		mid, err := Between(low, high)
		if err != nil {
			// Ожидаем ErrRankExhausted где-то после 20 итераций.
			require.ErrorIs(t, err, ErrRankExhausted, "iter %d", i)
			return
		}
		require.Less(t, mid, high, "iter %d", i)
		high = mid
	}
	t.Fatal("expected ErrRankExhausted after 30 halvings")
}

func TestLessThan(t *testing.T) {
	assert.True(t, LessThan("a", "b"))
	assert.False(t, LessThan("b", "a"))
	assert.False(t, LessThan("a", "a"))
}
