import type { Op } from './protocol';

// Undo/redo стек. Живёт в браузере, НЕ на сервере.
//
// Модель:
//   1. Пользователь делает действие (drag, color, create, delete, …).
//   2. Board «до» применения ops сохраняет «before» значения затронутых
//      props и строит «inverse»-ops — те же id, но с «до»-значениями.
//   3. Forward-ops применяются локально И отправляются на сервер.
//   4. {inverse, redo} кладётся в undo-стек.
//   5. Ctrl+Z: достать верхний frame, отправить inverse как ОБЫЧНЫЕ ops
//      с НОВЫМ TS. Для сервера и других клиентов undo = обычный upsert.
//
// Это принципиально: undo НЕ «откатывает историю», а создаёт новую
// операцию, которая «противоположна» по смыслу. LWW-merge на сервере
// корректно разберётся с тем, что чужие параллельные правки могли
// перехватить id между нашим действием и undo.

export interface HistoryFrame {
    // «inverse» — ops, применяющие ПРЕДШЕСТВУЮЩЕЕ состояние затронутых
    // свойств. TS у всех ops здесь = placeholder (w=0,l=0,n=''); его
    // подставит board.undo() через clock.now() перед отправкой.
    inverse: Op[];
    // «redo» — оригинальные ops (тоже с placeholder-TS). Отправляются
    // заново с новым TS при Ctrl+Shift+Z.
    redo: Op[];
}

export class UndoStack {
    private undo: HistoryFrame[] = [];
    private redo: HistoryFrame[] = [];

    record(f: HistoryFrame): void {
        if (f.inverse.length === 0 && f.redo.length === 0) return;
        this.undo.push(f);
        // Любое новое действие обнуляет redo-стек — стандартное
        // поведение «you can't redo after a fresh edit».
        this.redo.length = 0;
    }

    canUndo(): boolean {
        return this.undo.length > 0;
    }
    canRedo(): boolean {
        return this.redo.length > 0;
    }

    popUndo(): HistoryFrame | null {
        const f = this.undo.pop();
        if (!f) return null;
        this.redo.push(f);
        return f;
    }

    popRedo(): HistoryFrame | null {
        const f = this.redo.pop();
        if (!f) return null;
        this.undo.push(f);
        return f;
    }

    clear(): void {
        this.undo.length = 0;
        this.redo.length = 0;
    }

    depth(): { undo: number; redo: number } {
        return { undo: this.undo.length, redo: this.redo.length };
    }
}

// Placeholder TS — реальный TS подставится в board.emitWithHistory
// перед отправкой. Здесь держим uniform-форму, чтобы buildInverse и
// UndoStack имели одинаковый тип ops.
export const ZERO_TS = { w: 0, l: 0, n: '' } as const;
