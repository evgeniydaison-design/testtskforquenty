package crdt

import (
	"errors"
	"strings"
)

// Fractional index (LexoRank-lite).
//
// Rank — строка из алфавита base-36. Лексикографический порядок строк
// = z-order элементов. Между любыми A<B всегда можно вставить новую
// строку, что даёт вставку «между» без переиндексации соседей.
//
// Реализация упрощённая (см. «Limitations» ниже). Для Этапа 3 этого
// достаточно: типовой UX не порождает более 3–4 вставок в один и тот же
// интервал подряд. Полная «переалкация» (rebalance) — Этап 7+.
//
// Alphabet: цифры + строчные буквы. Важно: символы должны иметь
// consecutive code points, иначе midpoint ломается.
const (
	alphabet    = "0123456789abcdefghijklmnopqrstuvwxyz"
	alphabetLen = 36
	// midChar — «середина» алфавита по ИНДЕКСУ (18), не по байту.
	// Используется как стартовый символ и как «append»-расширение.
	midIdx      = alphabetLen / 2 // = 18 → 'i'
	maxRankLen  = 24              // защита от adversarial-разрастания
)

// RankError — signals «interval too narrow, rebalance required».
// Возвращается Between, когда midpoint совпадает с a (т.е. a и b уже
// соседние буквы). Клиент должен расширить суффикс — см. реализацию.
var ErrRankExhausted = errors.New("crdt: fractional rank interval exhausted")

// InitialRank — стартовый ранг для «первого элемента».
// Не «0» и не «z»: середина, чтобы и вставка «до», и «после» имели место.
func InitialRank() string { return string(alphabet[midIdx]) }

// indexOf — позиция символа в алфавите. Возвращает -1, если символа нет.
func indexOf(c byte) int { return strings.IndexByte(alphabet, c) }

// charAt — символ по индексу. Вызывающий обязан гарантировать 0<=i<L.
func charAt(i int) byte { return alphabet[i] }

// Between возвращает строку R, такую что a < R < b (лексикографически).
//
//	a == "" — вставить в начало (перед всем).
//	b == "" — вставить в конец (после всего).
//
// Возвращает ErrRankExhausted, если интервал слишком узкий.
//
// ВАЖНО: вся арифметика идёт по ИНДЕКСАМ алфавита, а не по байтам.
// Иначе «midpoint между '9' и 'a'» даст '~' (несуществующий символ),
// а «prepend перед 'i'» даст «ii» (> «i»), а не «9» (< «i»).
func Between(a, b string) (string, error) {
	if a == "" && b == "" {
		return InitialRank(), nil
	}
	if a == "" {
		return before(b)
	}
	if b == "" {
		return after(a)
	}
	return between(a, b)
}

// before — строка R, строго меньше b. R не пустая.
func before(b string) (string, error) {
	if b == "" {
		return "", ErrRankExhausted
	}
	if len(b) >= maxRankLen {
		return "", ErrRankExhausted
	}
	cb := indexOf(b[0])
	if cb < 0 {
		return "", ErrRankExhausted
	}
	if cb > 0 {
		// Midpoint между индексом 0 и cb. Если cb==1, mid=0 → '0'.
		return string(charAt(cb / 2)), nil
	}
	// b[0] == '0'. Нужен R = "0" + X, где X < b[1:].
	inner, err := before(b[1:])
	if err != nil {
		return "", err
	}
	return "0" + inner, nil
}

// after — строка R, строго больше a. Простейший вариант: расширить a
// midChar-ом. «a» — префикс «a+mid», значит «a» < «a+mid» ✓.
func after(a string) (string, error) {
	if len(a) >= maxRankLen {
		return "", ErrRankExhausted
	}
	return a + string(charAt(midIdx)), nil
}

// between — общий случай: a < R < b, обе непустые.
func between(a, b string) (string, error) {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		// Индекс символа в алфавите. Если строка короче i — «виртуальное
		// дополнение»: a дополняем минимумом (0), b — «после максимума».
		var ia, ib int
		if i < len(a) {
			ia = indexOf(a[i])
			if ia < 0 {
				return "", ErrRankExhausted
			}
		} else {
			ia = 0
		}
		if i < len(b) {
			ib = indexOf(b[i])
			if ib < 0 {
				return "", ErrRankExhausted
			}
		} else {
			ib = alphabetLen
		}
		if ia == ib {
			continue
		}
		if ib-ia > 1 {
			// Есть зазор — берём midpoint по индексу.
			mid := ia + (ib-ia)/2
			return a[:i] + string(charAt(mid)), nil
		}
		// Зазора нет (ia и ib соседние). «Проваливаемся» на уровень вниз.
		if i < len(a) {
			// a ещё не исчерпана: R = a[:i+1] + after(a[i+1:]).
			// a[:i+1] < R (расширение), и R < b (потому что R[i]==a[i]<b[i]).
			prefix := a[:i+1]
			rest := a[i+1:]
			if len(prefix)+len(rest)+1 >= maxRankLen {
				return "", ErrRankExhausted
			}
			return prefix + InitialRank(), nil
		}
		// a исчерпана на этом уровне. R = a + before(b[i:]).
		if len(a)+1 >= maxRankLen {
			return "", ErrRankExhausted
		}
		inner, err := before(b[i:])
		if err != nil {
			return "", err
		}
		return a + inner, nil
	}
	// a == b на всех позициях — интервал пуст.
	return "", ErrRankExhausted
}

// Before возвращает строку меньше min. Синоним Between("", min).
func Before(min string) (string, error) { return Between("", min) }

// After возвращает строку больше max. Синоним Between(max, "").
func After(max string) (string, error) { return Between(max, "") }

// LessThan — лексикографический «строго меньше».
func LessThan(a, b string) bool { return strings.Compare(a, b) < 0 }
