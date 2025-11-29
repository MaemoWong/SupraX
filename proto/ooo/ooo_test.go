package ooo

import (
	"math/bits"
	"testing"
	"unsafe"
)

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// SUPRAX Out-of-Order Scheduler - Test Suite
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// TEST PHILOSOPHY:
// ────────────────
// These tests serve dual purposes:
//   1. Functional verification: Ensure Go model behaves correctly
//   2. Hardware specification: Define expected RTL behavior
//
// When you write SystemVerilog, run these same test vectors against RTL.
// If Go and RTL produce identical outputs, the hardware is correct.
//
// WHAT WE'RE TESTING:
// ──────────────────
// An out-of-order (OoO) scheduler decides WHICH instructions execute WHEN.
// Modern CPUs don't execute instructions in program order - they find
// independent work and run it in parallel. This is how a 4GHz CPU
// achieves 12+ instructions per cycle despite most instructions
// taking multiple cycles to complete.
//
// The scheduler's job:
//   1. Track which instructions are waiting for data (dependencies)
//   2. Identify which instructions CAN execute (all inputs ready)
//   3. Pick the BEST instructions to execute (critical path first)
//   4. Update state when instructions complete
//
// KEY CONCEPTS FOR CPU NEWCOMERS:
// ──────────────────────────────
//
// SCOREBOARD:
//   A bitmap tracking which registers contain valid data.
//   Bit N = 1 means register N is "ready" (has valid data, not being written).
//   Bit N = 0 means register N is "pending" (being computed by in-flight op).
//
// DEPENDENCY MATRIX:
//   Tracks producer→consumer relationships within the scheduling window.
//   matrix[i] bit j = 1 means "instruction j depends on instruction i"
//   Used to ensure consumers wait for producers to ISSUE (not just complete).
//
// DEPENDENCY:
//   Instruction B depends on instruction A if B reads a register that A writes.
//   Example: A: R3 = R1 + R2    (writes R3)
//            B: R5 = R3 + R4    (reads R3 - depends on A!)
//   B cannot execute until A completes and R3 has valid data.
//
// RAW/WAR/WAW HAZARDS:
//   RAW (Read-After-Write): True dependency. B reads what A writes. Must wait.
//   WAR (Write-After-Read): Anti-dependency. B writes what A reads. Implicit in age.
//   WAW (Write-After-Write): Output dependency. B writes what A writes.
//        Handled by scoreboard[dest] check - younger blocked until older completes.
//
// ISSUE vs EXECUTE vs COMPLETE:
//   Issue:    Scheduler selects instruction, marks dest register pending
//   Execute:  Instruction runs in execution unit (1-20 cycles depending on op)
//   Complete: Execution finishes, dest register marked ready
//
// SLOT INDEX = AGE:
//   Instructions enter the window in program order.
//   Higher slot index = older instruction = entered earlier.
//   This is a "topological" property - no age field needed.
//   Slot 31 is oldest, Slot 0 is newest.
//
// CRITICAL PATH:
//   The longest chain of dependent instructions. Determines minimum execution time.
//   Example: A→B→C→D (4 instructions, each depending on previous)
//   Even with infinite parallelism, this chain takes 4 cycles minimum.
//   We prioritize critical path instructions to avoid unnecessary delays.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// TEST ORGANIZATION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// Tests are organized to mirror the hardware pipeline:
//
// 1. SCOREBOARD TESTS
//    Basic register readiness tracking
//
// 2. CYCLE 0 STAGE 1: Dependency Matrix
//    Which instructions block which others?
//
// 3. CYCLE 0 STAGE 2: Ready Bitmap
//    Which instructions have all sources ready, dest free, and producers issued?
//
// 4. CYCLE 0 STAGE 3: Priority Classification
//    Which ready instructions are on critical path?
//
// 5. CYCLE 1: Issue Selection
//    Pick up to 16 instructions to execute (with WAW conflict avoidance)
//
// 6. CYCLE 1: Scoreboard Updates
//    Mark registers pending/ready
//
// 7. PIPELINE INTEGRATION
//    Full 2-cycle pipeline behavior
//
// 8. DEPENDENCY PATTERNS
//    Chains, diamonds, forests, etc.
//
// 9. HAZARD HANDLING
//    RAW, WAR, WAW scenarios
//
// 10. EDGE CASES
//     Boundary conditions, corner cases
//
// 11. CORRECTNESS INVARIANTS
//     Properties that must ALWAYS hold
//
// 12. STRESS TESTS
//     High-volume, repeated operations
//
// 13. PIPELINE HAZARDS
//     Stale data between cycles
//
// 14. DOCUMENTATION TESTS
//     Verify assumptions, print specs
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 1. SCOREBOARD TESTS
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// The scoreboard is a 64-bit bitmap tracking register readiness.
// It's the simplest component but fundamental to everything else.
//
// Hardware: 64 flip-flops + read/write logic (~400 transistors)
// Timing: 20ps for read, 40ps for write
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestScoreboard_InitialState(t *testing.T) {
	// WHAT: Verify scoreboard starts with all registers not ready
	// WHY: Zero-initialized state represents "all registers pending"
	// HARDWARE: All flip-flops reset to 0

	var sb Scoreboard

	for i := uint8(0); i < 64; i++ {
		if sb.IsReady(i) {
			t.Errorf("Register %d should not be ready on init (scoreboard=0x%016X)", i, sb)
		}
	}

	if sb != 0 {
		t.Errorf("Initial scoreboard should be 0, got 0x%016X", sb)
	}
}

func TestScoreboard_MarkReady_Single(t *testing.T) {
	// WHAT: Mark one register ready, verify only that bit changes
	// WHY: Basic functionality - execution completes, register becomes valid
	// HARDWARE: OR gate sets single bit

	var sb Scoreboard

	sb.MarkReady(5)

	if !sb.IsReady(5) {
		t.Error("Register 5 should be ready after MarkReady")
	}

	if sb.IsReady(4) {
		t.Error("Register 4 should not be affected")
	}
	if sb.IsReady(6) {
		t.Error("Register 6 should not be affected")
	}

	expected := Scoreboard(1 << 5)
	if sb != expected {
		t.Errorf("Expected 0x%016X, got 0x%016X", expected, sb)
	}
}

func TestScoreboard_MarkPending_Single(t *testing.T) {
	// WHAT: Mark a ready register as pending
	// WHY: When instruction issues, its destination becomes pending (awaiting result)
	// HARDWARE: AND gate clears single bit

	var sb Scoreboard
	sb.MarkReady(5)

	sb.MarkPending(5)

	if sb.IsReady(5) {
		t.Error("Register 5 should be pending after MarkPending")
	}

	if sb != 0 {
		t.Errorf("Scoreboard should be 0 after marking only ready register pending, got 0x%016X", sb)
	}
}

func TestScoreboard_Idempotent(t *testing.T) {
	// WHAT: Calling MarkReady/MarkPending multiple times has no extra effect
	// WHY: Hardware naturally idempotent (OR 1 with 1 = 1, AND 0 with 0 = 0)
	// HARDWARE: No special handling needed

	var sb Scoreboard

	sb.MarkReady(10)
	sb.MarkReady(10)
	sb.MarkReady(10)

	if !sb.IsReady(10) {
		t.Error("Register should remain ready after multiple MarkReady")
	}

	expected := Scoreboard(1 << 10)
	if sb != expected {
		t.Errorf("Multiple MarkReady should not change value, expected 0x%016X, got 0x%016X", expected, sb)
	}

	sb.MarkPending(10)
	sb.MarkPending(10)
	sb.MarkPending(10)

	if sb.IsReady(10) {
		t.Error("Register should remain pending after multiple MarkPending")
	}

	if sb != 0 {
		t.Errorf("Multiple MarkPending should not change value, got 0x%016X", sb)
	}
}

func TestScoreboard_AllRegisters(t *testing.T) {
	// WHAT: Exercise all 64 registers
	// WHY: Verify no off-by-one errors, all bits accessible
	// HARDWARE: Validates full 64-bit datapath

	var sb Scoreboard

	for i := uint8(0); i < 64; i++ {
		sb.MarkReady(i)
	}

	for i := uint8(0); i < 64; i++ {
		if !sb.IsReady(i) {
			t.Errorf("Register %d should be ready", i)
		}
	}

	if sb != ^Scoreboard(0) {
		t.Errorf("All registers ready should be 0xFFFFFFFFFFFFFFFF, got 0x%016X", sb)
	}

	for i := uint8(0); i < 64; i++ {
		sb.MarkPending(i)
	}

	for i := uint8(0); i < 64; i++ {
		if sb.IsReady(i) {
			t.Errorf("Register %d should be pending", i)
		}
	}

	if sb != 0 {
		t.Errorf("All registers pending should be 0, got 0x%016X", sb)
	}
}

func TestScoreboard_BoundaryRegisters(t *testing.T) {
	// WHAT: Test register 0 (LSB) and register 63 (MSB)
	// WHY: Boundary conditions often harbor bugs (off-by-one, sign extension)
	// HARDWARE: Validates bit indexing at extremes

	var sb Scoreboard

	sb.MarkReady(0)
	if !sb.IsReady(0) {
		t.Error("Register 0 should be ready")
	}
	if sb != 1 {
		t.Errorf("Only bit 0 should be set, got 0x%016X", sb)
	}

	sb.MarkPending(0)
	if sb.IsReady(0) {
		t.Error("Register 0 should be pending")
	}

	sb.MarkReady(63)
	if !sb.IsReady(63) {
		t.Error("Register 63 should be ready")
	}

	expected := Scoreboard(1 << 63)
	if sb != expected {
		t.Errorf("Only bit 63 should be set, expected 0x%016X, got 0x%016X", expected, sb)
	}

	sb.MarkPending(63)
	if sb.IsReady(63) {
		t.Error("Register 63 should be pending")
	}
}

func TestScoreboard_InterleavedPattern(t *testing.T) {
	// WHAT: Set alternating bits (checkerboard pattern)
	// WHY: Tests bit independence, no crosstalk between adjacent bits
	// HARDWARE: Validates isolation between flip-flops

	var sb Scoreboard

	for i := uint8(0); i < 64; i += 2 {
		sb.MarkReady(i)
	}

	for i := uint8(0); i < 64; i++ {
		expected := (i % 2) == 0
		if sb.IsReady(i) != expected {
			t.Errorf("Register %d: expected ready=%v, got ready=%v", i, expected, sb.IsReady(i))
		}
	}

	expected := Scoreboard(0x5555555555555555)
	if sb != expected {
		t.Errorf("Checkerboard pattern wrong, expected 0x%016X, got 0x%016X", expected, sb)
	}
}

func TestScoreboard_IndependentUpdates(t *testing.T) {
	// WHAT: Updates to one register don't affect others
	// WHY: Critical correctness property - false dependencies would break everything
	// HARDWARE: Validates per-bit isolation

	var sb Scoreboard

	sb.MarkReady(10)
	sb.MarkReady(20)
	sb.MarkReady(30)

	sb.MarkPending(20)

	if !sb.IsReady(10) {
		t.Error("Register 10 should still be ready")
	}
	if sb.IsReady(20) {
		t.Error("Register 20 should be pending")
	}
	if !sb.IsReady(30) {
		t.Error("Register 30 should still be ready")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 2. CYCLE 0 STAGE 1: DEPENDENCY MATRIX
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// BuildDependencyMatrix identifies producer→consumer relationships.
// matrix[i] bit j = 1 means "instruction j depends on instruction i"
//
// A dependency exists when:
//   1. Consumer reads what producer writes (RAW hazard)
//   2. Producer is older than consumer (slot index comparison)
//
// The slot index check is CRITICAL:
//   - Higher slot = older instruction (entered window earlier)
//   - Producer must be older to create valid dependency
//   - This prevents false WAR dependencies
//
// Hardware: 1024 parallel XOR comparators (32×32 matrix)
// Timing: 120ps (XOR + zero detect + age compare + AND)
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestDependencyMatrix_Empty(t *testing.T) {
	// WHAT: No valid instructions → empty matrix
	// WHY: Base case - no instructions means no dependencies
	// HARDWARE: All valid bits 0, all outputs 0

	window := &InstructionWindow{}

	matrix := BuildDependencyMatrix(window)

	for i := 0; i < 32; i++ {
		if matrix[i] != 0 {
			t.Errorf("Empty window should have no dependencies, row %d = 0x%08X", i, matrix[i])
		}
	}
}

func TestDependencyMatrix_NoDependencies(t *testing.T) {
	// WHAT: Multiple instructions, none depending on each other
	// WHY: Independent instructions - maximum parallelism case
	// HARDWARE: All XOR comparisons produce non-zero (no match)

	window := &InstructionWindow{}

	window.Ops[2] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[1] = Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11}
	window.Ops[0] = Operation{Valid: true, Src1: 5, Src2: 6, Dest: 12}

	matrix := BuildDependencyMatrix(window)

	for i := 0; i < 32; i++ {
		if matrix[i] != 0 {
			t.Errorf("Independent ops should have no dependencies, row %d = 0x%08X", i, matrix[i])
		}
	}
}

func TestDependencyMatrix_SimpleRAW(t *testing.T) {
	// WHAT: Basic Read-After-Write dependency
	// WHY: Core functionality - this is the dependency we track
	// HARDWARE: XOR produces zero, age check passes
	//
	// EXAMPLE:
	//   Slot 10 (older): R10 = R1 + R2     (writes R10)
	//   Slot 5 (newer):  R11 = R10 + R3    (reads R10 - depends on slot 10!)

	window := &InstructionWindow{}

	window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[5] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}

	matrix := BuildDependencyMatrix(window)

	if (matrix[10]>>5)&1 != 1 {
		t.Errorf("Slot 5 should depend on slot 10, matrix[10]=0x%08X", matrix[10])
	}

	if (matrix[5]>>10)&1 != 0 {
		t.Errorf("Slot 10 should NOT depend on slot 5, matrix[5]=0x%08X", matrix[5])
	}
}

func TestDependencyMatrix_RAW_Src1(t *testing.T) {
	// WHAT: Consumer reads producer's output via Src1
	// WHY: Test both source paths independently
	// HARDWARE: XOR(Src1, Dest) produces zero

	window := &InstructionWindow{}

	window.Ops[15] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 20}
	window.Ops[10] = Operation{Valid: true, Src1: 20, Src2: 3, Dest: 21}

	matrix := BuildDependencyMatrix(window)

	if (matrix[15]>>10)&1 != 1 {
		t.Errorf("RAW via Src1 not detected, matrix[15]=0x%08X", matrix[15])
	}
}

func TestDependencyMatrix_RAW_Src2(t *testing.T) {
	// WHAT: Consumer reads producer's output via Src2
	// WHY: Test both source paths independently
	// HARDWARE: XOR(Src2, Dest) produces zero

	window := &InstructionWindow{}

	window.Ops[15] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 20}
	window.Ops[10] = Operation{Valid: true, Src1: 3, Src2: 20, Dest: 21}

	matrix := BuildDependencyMatrix(window)

	if (matrix[15]>>10)&1 != 1 {
		t.Errorf("RAW via Src2 not detected, matrix[15]=0x%08X", matrix[15])
	}
}

func TestDependencyMatrix_RAW_BothSources(t *testing.T) {
	// WHAT: Consumer reads producer's output via both sources
	// WHY: Some instructions use same source twice (e.g., R5 = R3 * R3)
	// HARDWARE: Both XORs produce zero, OR combines them

	window := &InstructionWindow{}

	window.Ops[15] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 20}
	window.Ops[10] = Operation{Valid: true, Src1: 20, Src2: 20, Dest: 21}

	matrix := BuildDependencyMatrix(window)

	if (matrix[15]>>10)&1 != 1 {
		t.Errorf("RAW via both sources not detected, matrix[15]=0x%08X", matrix[15])
	}

	if bits.OnesCount32(matrix[15]) != 1 {
		t.Errorf("Should be exactly one dependent, got %d", bits.OnesCount32(matrix[15]))
	}
}

func TestDependencyMatrix_Chain(t *testing.T) {
	// WHAT: Linear dependency chain A→B→C
	// WHY: Common pattern - sequential computation
	// HARDWARE: Creates two separate dependencies
	//
	// EXAMPLE:
	//   Slot 20 (oldest): A writes R10
	//   Slot 15 (middle): B reads R10, writes R11
	//   Slot 10 (newest): C reads R11

	window := &InstructionWindow{}

	window.Ops[20] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[15] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	window.Ops[10] = Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12}

	matrix := BuildDependencyMatrix(window)

	if (matrix[20]>>15)&1 != 1 {
		t.Errorf("B should depend on A, matrix[20]=0x%08X", matrix[20])
	}

	if (matrix[15]>>10)&1 != 1 {
		t.Errorf("C should depend on B, matrix[15]=0x%08X", matrix[15])
	}

	if matrix[10] != 0 {
		t.Errorf("C should have no dependents, matrix[10]=0x%08X", matrix[10])
	}

	if (matrix[20]>>10)&1 != 0 {
		t.Errorf("C should NOT directly depend on A, matrix[20]=0x%08X", matrix[20])
	}
}

func TestDependencyMatrix_Diamond(t *testing.T) {
	// WHAT: Diamond dependency pattern A→{B,C}→D
	// WHY: Common in expressions like D = f(B(A), C(A))
	// HARDWARE: A has two dependents, B and C each have one
	//
	// VISUAL:
	//       A (slot 25)
	//      / \
	//     B   C (slots 20, 15)
	//      \ /
	//       D (slot 10)

	window := &InstructionWindow{}

	window.Ops[25] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[20] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	window.Ops[15] = Operation{Valid: true, Src1: 10, Src2: 4, Dest: 12}
	window.Ops[10] = Operation{Valid: true, Src1: 11, Src2: 12, Dest: 13}

	matrix := BuildDependencyMatrix(window)

	if (matrix[25]>>20)&1 != 1 {
		t.Errorf("B should depend on A")
	}
	if (matrix[25]>>15)&1 != 1 {
		t.Errorf("C should depend on A")
	}

	if (matrix[20]>>10)&1 != 1 {
		t.Errorf("D should depend on B")
	}

	if (matrix[15]>>10)&1 != 1 {
		t.Errorf("D should depend on C")
	}

	if matrix[10] != 0 {
		t.Errorf("D should have no dependents")
	}
}

func TestDependencyMatrix_MultipleConsumers(t *testing.T) {
	// WHAT: One producer, many consumers
	// WHY: Common pattern - computed value used multiple times
	// HARDWARE: Producer's row has multiple bits set

	window := &InstructionWindow{}

	window.Ops[25] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	window.Ops[20] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	window.Ops[15] = Operation{Valid: true, Src1: 10, Src2: 4, Dest: 12}
	window.Ops[10] = Operation{Valid: true, Src1: 10, Src2: 5, Dest: 13}
	window.Ops[5] = Operation{Valid: true, Src1: 10, Src2: 6, Dest: 14}

	matrix := BuildDependencyMatrix(window)

	expected := uint32((1 << 20) | (1 << 15) | (1 << 10) | (1 << 5))
	if matrix[25] != expected {
		t.Errorf("Expected 4 dependents, got matrix[25]=0x%08X", matrix[25])
	}
}

func TestDependencyMatrix_AgeCheck_PreventsFalseDependency(t *testing.T) {
	// WHAT: Newer instruction writes register that older instruction reads
	// WHY: This is WAR (anti-dependency), NOT a true dependency
	// HARDWARE: Age check (i > j) prevents false positive
	//
	// CRITICAL TEST:
	//   Slot 15 (older): reads R5
	//   Slot 5 (newer):  writes R5
	//
	// Without age check: "Slot 15 reads R5, Slot 5 writes R5" → false dependency!
	// With age check: 5 > 15 is FALSE → no dependency ✓

	window := &InstructionWindow{}

	window.Ops[15] = Operation{Valid: true, Src1: 5, Src2: 6, Dest: 10}
	window.Ops[5] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5}

	matrix := BuildDependencyMatrix(window)

	if (matrix[5]>>15)&1 != 0 {
		t.Errorf("Age check should prevent WAR dependency, matrix[5]=0x%08X", matrix[5])
	}

	if matrix[5] != 0 || matrix[15] != 0 {
		t.Errorf("No dependencies should exist (WAR not tracked)")
	}
}

func TestDependencyMatrix_SlotIndexBoundaries(t *testing.T) {
	// WHAT: Dependencies between slot 31 (oldest) and slot 0 (newest)
	// WHY: Maximum slot index difference, validates comparison logic
	// HARDWARE: 5-bit comparison at extremes

	window := &InstructionWindow{}

	window.Ops[31] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[0] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}

	matrix := BuildDependencyMatrix(window)

	if (matrix[31]>>0)&1 != 1 {
		t.Errorf("Slot 0 should depend on slot 31, matrix[31]=0x%08X", matrix[31])
	}
}

func TestDependencyMatrix_AdjacentSlots(t *testing.T) {
	// WHAT: Dependency between adjacent slots
	// WHY: Minimum slot index difference (off-by-one check)
	// HARDWARE: Age check must handle i = j+1

	window := &InstructionWindow{}

	window.Ops[11] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[10] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}

	matrix := BuildDependencyMatrix(window)

	if (matrix[11]>>10)&1 != 1 {
		t.Errorf("Adjacent slot dependency not detected, matrix[11]=0x%08X", matrix[11])
	}
}

func TestDependencyMatrix_DiagonalZero(t *testing.T) {
	// WHAT: No instruction depends on itself
	// WHY: Self-dependency is impossible (can't read own output before producing it)
	// HARDWARE: i == j case skipped

	window := &InstructionWindow{}

	for i := 0; i < 10; i++ {
		window.Ops[i] = Operation{
			Valid: true,
			Src1:  uint8(i + 10),
			Src2:  uint8(i + 10),
			Dest:  uint8(i + 10),
		}
	}

	matrix := BuildDependencyMatrix(window)

	for i := 0; i < 10; i++ {
		if (matrix[i]>>i)&1 != 0 {
			t.Errorf("Diagonal matrix[%d][%d] should be 0", i, i)
		}
	}
}

func TestDependencyMatrix_InvalidOps(t *testing.T) {
	// WHAT: Invalid ops don't create dependencies
	// WHY: Empty slots shouldn't participate in dependency tracking
	// HARDWARE: valid=0 gates both comparisons to 0

	window := &InstructionWindow{}

	window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[5] = Operation{Valid: false, Src1: 10, Src2: 3, Dest: 11}

	matrix := BuildDependencyMatrix(window)

	if matrix[10] != 0 {
		t.Errorf("Invalid ops shouldn't create dependencies, matrix[10]=0x%08X", matrix[10])
	}
}

func TestDependencyMatrix_InvalidProducer(t *testing.T) {
	// WHAT: Invalid producer can't have dependents
	// WHY: Symmetric with invalid consumer test
	// HARDWARE: valid=0 skips entire row computation

	window := &InstructionWindow{}

	window.Ops[10] = Operation{Valid: false, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[5] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}

	matrix := BuildDependencyMatrix(window)

	if matrix[10] != 0 {
		t.Errorf("Invalid producer shouldn't have dependents, matrix[10]=0x%08X", matrix[10])
	}
}

func TestDependencyMatrix_ComplexGraph(t *testing.T) {
	// WHAT: Complex dependency pattern with multiple paths
	// WHY: Realistic workload - instruction-level parallelism mixed with chains
	// HARDWARE: Full stress of parallel comparator array
	//
	// GRAPH:
	//       A (slot 31)
	//      /|\
	//     B C D (slots 28, 25, 22)
	//     |X|/
	//     E F (slots 19, 16)
	//      \|
	//       G (slot 13)

	window := &InstructionWindow{}

	window.Ops[31] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[28] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	window.Ops[25] = Operation{Valid: true, Src1: 10, Src2: 4, Dest: 12}
	window.Ops[22] = Operation{Valid: true, Src1: 10, Src2: 5, Dest: 13}
	window.Ops[19] = Operation{Valid: true, Src1: 11, Src2: 12, Dest: 14}
	window.Ops[16] = Operation{Valid: true, Src1: 12, Src2: 13, Dest: 15}
	window.Ops[13] = Operation{Valid: true, Src1: 14, Src2: 15, Dest: 16}

	matrix := BuildDependencyMatrix(window)

	if (matrix[31]>>28)&1 != 1 || (matrix[31]>>25)&1 != 1 || (matrix[31]>>22)&1 != 1 {
		t.Errorf("A should have B,C,D as dependents, matrix[31]=0x%08X", matrix[31])
	}

	if (matrix[28]>>19)&1 != 1 {
		t.Errorf("E should depend on B")
	}
	if (matrix[25]>>19)&1 != 1 {
		t.Errorf("E should depend on C")
	}

	if matrix[13] != 0 {
		t.Errorf("G should be leaf, matrix[13]=0x%08X", matrix[13])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 3. CYCLE 0 STAGE 2: READY BITMAP
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// ComputeReadyBitmap identifies which instructions can issue right now.
// An instruction is ready when ALL of these are true:
//   1. It's valid (slot contains real instruction)
//   2. It's not already issued (prevents double-issue)
//   3. Both source registers are ready (RAW completion check)
//   4. Destination register is ready (WAW check)
//   5. All intra-window producers have issued (RAW issue check)
//
// The combination of scoreboard AND dependency matrix is essential:
//   - Scoreboard: Tracks completed writes (data validity)
//   - Dependency matrix + Issued: Tracks issued but incomplete writes
//
// Hardware: 32 parallel checkers, each doing scoreboard lookups + producer scan
// Timing: 150ps
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestReadyBitmap_EmptyWindow(t *testing.T) {
	// WHAT: No valid instructions → no ready instructions
	// WHY: Base case - empty scheduler should produce no work
	// HARDWARE: All valid bits are 0, so all ready bits are 0

	window := &InstructionWindow{}
	var sb Scoreboard
	depMatrix := BuildDependencyMatrix(window)

	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 0 {
		t.Errorf("Empty window should have no ready ops, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_SingleReady(t *testing.T) {
	// WHAT: One instruction with sources and dest ready, no dependencies
	// WHY: Simplest positive case
	// HARDWARE: One ready checker outputs 1

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  5,
		Src2:  10,
		Dest:  15,
	}
	sb.MarkReady(5)
	sb.MarkReady(10)
	sb.MarkReady(15)

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 1 {
		t.Errorf("Single ready op at slot 0 should give bitmap 0x1, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_MultipleReady(t *testing.T) {
	// WHAT: Multiple independent instructions, all ready
	// WHY: Verify parallel operation - all checkers work simultaneously
	// HARDWARE: Multiple ready checkers output 1

	window := &InstructionWindow{}
	var sb Scoreboard

	for i := 0; i < 3; i++ {
		window.Ops[i] = Operation{
			Valid: true,
			Src1:  1,
			Src2:  2,
			Dest:  uint8(10 + i),
		}
	}
	sb.MarkReady(1)
	sb.MarkReady(2)
	sb.MarkReady(10)
	sb.MarkReady(11)
	sb.MarkReady(12)

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	expected := uint32(0b111)
	if bitmap != expected {
		t.Errorf("Expected bitmap 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_Src1NotReady(t *testing.T) {
	// WHAT: Instruction blocked on Src1
	// WHY: Verify AND logic - all conditions must be true
	// HARDWARE: ready = valid & ~issued & src1Ready & src2Ready & destReady & noUnissuedProducers

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  5,  // Not ready
		Src2:  10, // Ready
		Dest:  15, // Ready
	}
	sb.MarkReady(10)
	sb.MarkReady(15)
	// Src1 (5) NOT ready

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 0 {
		t.Errorf("Op with Src1 not ready should not be in bitmap, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_Src2NotReady(t *testing.T) {
	// WHAT: Instruction blocked on Src2
	// WHY: Symmetric with Src1 test
	// HARDWARE: Same AND logic, different input

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  5,  // Ready
		Src2:  10, // Not ready
		Dest:  15, // Ready
	}
	sb.MarkReady(5)
	sb.MarkReady(15)
	// Src2 (10) NOT ready

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 0 {
		t.Errorf("Op with Src2 not ready should not be in bitmap, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_DestNotReady_WAW(t *testing.T) {
	// WHAT: Instruction blocked because dest is being written (WAW)
	// WHY: This is the KEY test for WAW handling via scoreboard
	// HARDWARE: destReady = scoreboard[dest] blocks younger WAW
	//
	// SCENARIO:
	//   Some older instruction is writing R15 (scoreboard[15] = 0)
	//   Our instruction also wants to write R15
	//   We must wait until older instruction completes

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  5,  // Ready
		Src2:  10, // Ready
		Dest:  15, // NOT ready (older instruction writing it)
	}
	sb.MarkReady(5)
	sb.MarkReady(10)
	// Dest (15) NOT ready - simulates older instruction writing R15

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 0 {
		t.Errorf("Op with dest not ready (WAW) should not be in bitmap, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_BothSourcesNotReady(t *testing.T) {
	// WHAT: Instruction blocked on both sources
	// WHY: Complete coverage of blocked states
	// HARDWARE: Both scoreboard lookups return 0

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  5,
		Src2:  10,
		Dest:  15,
	}
	sb.MarkReady(15) // Only dest ready
	// Neither source ready

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 0 {
		t.Errorf("Op with no sources ready should not be in bitmap, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_InvalidOps(t *testing.T) {
	// WHAT: Invalid ops (empty slots) never appear in bitmap
	// WHY: Empty slots shouldn't be scheduled
	// HARDWARE: valid=0 gates the output to 0

	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0) // All registers ready

	// All ops invalid (default)
	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 0 {
		t.Errorf("Invalid ops should not be ready, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_SkipsIssuedOps(t *testing.T) {
	// WHAT: Already-issued ops excluded from ready bitmap
	// WHY: Prevents double-issue - catastrophic bug if same instruction runs twice
	// HARDWARE: issued=1 gates the output to 0

	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0)

	window.Ops[0] = Operation{
		Valid:  true,
		Issued: true, // Already sent to execution
		Src1:   1,
		Src2:   2,
		Dest:   10,
	}

	window.Ops[1] = Operation{
		Valid:  true,
		Issued: false,
		Src1:   1,
		Src2:   2,
		Dest:   11,
	}

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	expected := uint32(0b10) // Only op 1
	if bitmap != expected {
		t.Errorf("Should skip issued ops, expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_SameSourceRegisters(t *testing.T) {
	// WHAT: Instruction using same register for both sources
	// WHY: Edge case - some instructions read one register twice (e.g., R5 = R3 * R3)
	// HARDWARE: Both MUXes select same scoreboard bit, AND still works

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  5,
		Src2:  5, // Same as Src1
		Dest:  10,
	}
	sb.MarkReady(5)
	sb.MarkReady(10)

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 1 {
		t.Errorf("Op with same source registers should be ready, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_SourceEqualsDestination(t *testing.T) {
	// WHAT: Instruction reads and writes same register (e.g., R5 = R5 + 1)
	// WHY: Common pattern (increment), must work correctly
	// HARDWARE: All three checks (src1, src2, dest) reference same scoreboard bit
	//
	// KEY: If R5 is ready, we can both read it AND claim it for writing.
	// The read happens before the write in instruction semantics.

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  5,
		Src2:  5,
		Dest:  5, // Same as sources
	}
	sb.MarkReady(5)

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 1 {
		t.Errorf("Op reading and writing same register should be ready, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_FullWindow(t *testing.T) {
	// WHAT: All 32 slots filled and ready
	// WHY: Maximum parallelism case - stress test parallel checkers
	// HARDWARE: All 32 ready checkers active simultaneously

	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0) // All registers ready

	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{
			Valid: true,
			Src1:  1,
			Src2:  2,
			Dest:  uint8(10 + i), // Different dests
		}
	}

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	expected := ^uint32(0) // All 32 bits set
	if bitmap != expected {
		t.Errorf("Full window should have all bits set, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_ScatteredSlots(t *testing.T) {
	// WHAT: Ready ops at non-contiguous slots
	// WHY: Real workloads have gaps (some ops issued, some blocked)
	// HARDWARE: Validates independence of slot checkers

	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0)

	slots := []int{0, 5, 10, 15, 20, 25, 30}
	for _, slot := range slots {
		window.Ops[slot] = Operation{
			Valid: true,
			Src1:  1,
			Src2:  2,
			Dest:  uint8(slot + 10),
		}
	}

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	var expected uint32
	for _, slot := range slots {
		expected |= 1 << slot
	}

	if bitmap != expected {
		t.Errorf("Scattered slots: expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_MixedReadiness(t *testing.T) {
	// WHAT: Mix of ready, blocked on src1, blocked on src2, blocked on dest
	// WHY: Realistic scenario - different ops have different dependencies
	// HARDWARE: Each checker operates independently

	window := &InstructionWindow{}
	var sb Scoreboard

	sb.MarkReady(1)
	sb.MarkReady(2)
	sb.MarkReady(10)
	sb.MarkReady(14)
	// Registers 3, 4, 11, 12, 13 NOT ready

	// Slot 0: All ready → READY
	window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	// Slot 1: Src1 ready, Src2 not → BLOCKED
	window.Ops[1] = Operation{Valid: true, Src1: 1, Src2: 3, Dest: 11}

	// Slot 2: Src1 not, Src2 ready → BLOCKED
	window.Ops[2] = Operation{Valid: true, Src1: 4, Src2: 2, Dest: 12}

	// Slot 3: Sources ready, Dest not (WAW) → BLOCKED
	window.Ops[3] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 13}

	// Slot 4: All ready → READY
	window.Ops[4] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 14}

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	expected := uint32(0b10001) // Slots 0 and 4
	if bitmap != expected {
		t.Errorf("Mixed readiness: expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_Register0(t *testing.T) {
	// WHAT: Operations using register 0
	// WHY: Register 0 is LSB boundary
	// HARDWARE: Validates bit 0 of scoreboard accessible

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  0,
		Src2:  0,
		Dest:  0,
	}
	sb.MarkReady(0)

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 1 {
		t.Errorf("Op using register 0 should be ready, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_Register63(t *testing.T) {
	// WHAT: Operations using highest register
	// WHY: Boundary condition - validates full register file accessible
	// HARDWARE: Validates bit 63 of scoreboard accessible

	window := &InstructionWindow{}
	var sb Scoreboard

	window.Ops[0] = Operation{
		Valid: true,
		Src1:  63,
		Src2:  63,
		Dest:  63,
	}
	sb.MarkReady(63)

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 1 {
		t.Errorf("Op using register 63 should be ready, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_UnissuedProducerBlocks(t *testing.T) {
	// WHAT: Consumer blocked by unissued producer in window
	// WHY: This is the KEY test for dependency matrix integration
	// HARDWARE: Producer scan blocks consumer even if scoreboard shows ready
	//
	// SCENARIO:
	//   Slot 10 (older): A writes R5 (not issued)
	//   Slot 5 (newer):  B reads R5 (depends on A)
	//
	// Even though scoreboard[5] = 1 (old data), B must wait for A to issue!

	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0) // All registers "ready"

	// Producer at slot 10 (older, not issued)
	window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5}

	// Consumer at slot 5 (younger, reads R5)
	window.Ops[5] = Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6}

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	// Only slot 10 should be ready (producer)
	// Slot 5 blocked by unissued producer
	expected := uint32(1 << 10)
	if bitmap != expected {
		t.Errorf("Consumer should be blocked by unissued producer, expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_IssuedProducerDoesNotBlock(t *testing.T) {
	// WHAT: Consumer not blocked by ISSUED producer (scoreboard takes over)
	// WHY: Once producer issues, scoreboard[dest] = 0 handles waiting
	// HARDWARE: Producer scan only blocks if producer.Issued = false
	//
	// SCENARIO:
	//   Slot 10 (older): A writes R5 (ISSUED, so scoreboard[5] = 0)
	//   Slot 5 (newer):  B reads R5
	//
	// A.Issued = true → B passes producer check
	// scoreboard[5] = 0 → B fails scoreboard check
	// B correctly waits for A to complete

	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0)

	// Producer at slot 10 (ISSUED)
	window.Ops[10] = Operation{Valid: true, Issued: true, Src1: 1, Src2: 2, Dest: 5}
	sb.MarkPending(5) // Producer has issued, dest is pending

	// Consumer at slot 5
	window.Ops[5] = Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6}

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	// Slot 10 already issued, not in bitmap
	// Slot 5 blocked by scoreboard[5] = 0 (not by producer check)
	if bitmap != 0 {
		t.Errorf("Consumer should be blocked by scoreboard, got 0x%08X", bitmap)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 4. CYCLE 0 STAGE 3: PRIORITY CLASSIFICATION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// ClassifyPriority splits ready instructions into two tiers:
//   - High Priority: Instructions with dependents (blocking other work)
//   - Low Priority:  Instructions without dependents (leaves)
//
// This approximates critical path scheduling:
//   - Schedule blockers first to unblock dependent work ASAP
//   - Leaves can wait without delaying anything
//
// Hardware: 32 parallel OR reduction trees
// Timing: 100ps (5-level OR tree per row)
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestPriority_AllLeaves(t *testing.T) {
	// WHAT: All ready ops have no dependents
	// WHY: Fully independent workload - all low priority
	// HARDWARE: All OR trees output 0

	readyBitmap := uint32(0b1111)
	depMatrix := DependencyMatrix{0, 0, 0, 0}

	priority := ClassifyPriority(readyBitmap, depMatrix)

	if priority.HighPriority != 0 {
		t.Errorf("All leaves should have no high priority, got 0x%08X", priority.HighPriority)
	}
	if priority.LowPriority != readyBitmap {
		t.Errorf("All leaves should be low priority, got 0x%08X", priority.LowPriority)
	}
}

func TestPriority_AllCritical(t *testing.T) {
	// WHAT: All ready ops have dependents
	// WHY: Fully serialized workload - all high priority
	// HARDWARE: All OR trees output 1

	readyBitmap := uint32(0b111)
	depMatrix := DependencyMatrix{
		0b010,
		0b100,
		0b001,
	}

	priority := ClassifyPriority(readyBitmap, depMatrix)

	if priority.HighPriority != readyBitmap {
		t.Errorf("All critical should be high priority, got 0x%08X", priority.HighPriority)
	}
	if priority.LowPriority != 0 {
		t.Errorf("No leaves expected, got low priority 0x%08X", priority.LowPriority)
	}
}

func TestPriority_Mixed(t *testing.T) {
	// WHAT: Mix of critical and leaf ops
	// WHY: Realistic scenario
	// HARDWARE: Some OR trees output 1, others output 0

	readyBitmap := uint32(0b11111)
	depMatrix := DependencyMatrix{
		0b00010, // Op 0 has op 1 as dependent → HIGH
		0b00000, // Op 1 no dependents → LOW
		0b01000, // Op 2 has op 3 as dependent → HIGH
		0b00000, // Op 3 no dependents → LOW
		0b00000, // Op 4 no dependents → LOW
	}

	priority := ClassifyPriority(readyBitmap, depMatrix)

	expectedHigh := uint32(0b00101)
	expectedLow := uint32(0b11010)

	if priority.HighPriority != expectedHigh {
		t.Errorf("High priority: expected 0x%08X, got 0x%08X", expectedHigh, priority.HighPriority)
	}
	if priority.LowPriority != expectedLow {
		t.Errorf("Low priority: expected 0x%08X, got 0x%08X", expectedLow, priority.LowPriority)
	}
}

func TestPriority_OnlyClassifiesReadyOps(t *testing.T) {
	// WHAT: Non-ready ops not classified even if they have dependents
	// WHY: Can't issue non-ready ops, so priority irrelevant
	// HARDWARE: ready bitmap gates the output

	readyBitmap := uint32(0b001)
	depMatrix := DependencyMatrix{
		0b010,
		0b100,
		0b000,
	}

	priority := ClassifyPriority(readyBitmap, depMatrix)

	if priority.HighPriority != 1 {
		t.Errorf("Only ready ops classified, expected high 0x1, got 0x%08X", priority.HighPriority)
	}
	if priority.LowPriority != 0 {
		t.Errorf("Only ready ops classified, expected low 0x0, got 0x%08X", priority.LowPriority)
	}
}

func TestPriority_EmptyReadyBitmap(t *testing.T) {
	// WHAT: No ready ops → empty priority classes
	// WHY: Nothing to classify
	// HARDWARE: All outputs gated to 0

	readyBitmap := uint32(0)
	depMatrix := DependencyMatrix{0b111, 0b111, 0b111}

	priority := ClassifyPriority(readyBitmap, depMatrix)

	if priority.HighPriority != 0 || priority.LowPriority != 0 {
		t.Error("Empty ready bitmap should produce empty priority classes")
	}
}

func TestPriority_DependentNotReady(t *testing.T) {
	// WHAT: Ready op has non-ready dependent
	// WHY: The dependent exists in matrix, affects classification
	// HARDWARE: OR tree sees 1 bit even if that dependent isn't ready

	readyBitmap := uint32(0b001)
	depMatrix := DependencyMatrix{
		0b010, // Op 0 has op 1 as dependent (but op 1 not ready)
	}

	priority := ClassifyPriority(readyBitmap, depMatrix)

	if priority.HighPriority != 1 {
		t.Errorf("Op with non-ready dependent still high priority, got 0x%08X", priority.HighPriority)
	}
}

func TestPriority_ChainClassification(t *testing.T) {
	// WHAT: Dependency chain A→B→C, all ready
	// WHY: A and B have dependents (high), C is leaf (low)
	// HARDWARE: Shows critical path identification

	readyBitmap := uint32(0b111)
	depMatrix := DependencyMatrix{
		0b010, // A → B
		0b100, // B → C
		0b000, // C is leaf
	}

	priority := ClassifyPriority(readyBitmap, depMatrix)

	expectedHigh := uint32(0b011)
	expectedLow := uint32(0b100)

	if priority.HighPriority != expectedHigh {
		t.Errorf("Chain high priority: expected 0x%08X, got 0x%08X", expectedHigh, priority.HighPriority)
	}
	if priority.LowPriority != expectedLow {
		t.Errorf("Chain low priority: expected 0x%08X, got 0x%08X", expectedLow, priority.LowPriority)
	}
}

func TestPriority_FullWindow(t *testing.T) {
	// WHAT: All 32 slots ready with various dependencies
	// WHY: Maximum scale test
	// HARDWARE: All 32 OR trees active

	readyBitmap := ^uint32(0)
	var depMatrix DependencyMatrix

	for i := 0; i < 31; i += 2 {
		depMatrix[i] = 1 << (i + 1)
	}

	priority := ClassifyPriority(readyBitmap, depMatrix)

	expectedHigh := uint32(0x55555555)
	expectedLow := uint32(0xAAAAAAAA)

	if priority.HighPriority != expectedHigh {
		t.Errorf("Full window high: expected 0x%08X, got 0x%08X", expectedHigh, priority.HighPriority)
	}
	if priority.LowPriority != expectedLow {
		t.Errorf("Full window low: expected 0x%08X, got 0x%08X", expectedLow, priority.LowPriority)
	}
}

func TestPriority_DisjointSets(t *testing.T) {
	// WHAT: High and low priority sets don't overlap
	// WHY: Each op is exactly one of: high, low, or not ready
	// HARDWARE: Mutual exclusion guaranteed by logic

	for _, readyBitmap := range []uint32{0, 0xFF, 0xFF00, 0xFFFFFFFF} {
		var depMatrix DependencyMatrix
		for i := 0; i < 32; i++ {
			if i%3 == 0 {
				depMatrix[i] = 1 << ((i + 7) % 32)
			}
		}

		priority := ClassifyPriority(readyBitmap, depMatrix)

		if priority.HighPriority&priority.LowPriority != 0 {
			t.Errorf("High and low overlap: H=0x%08X L=0x%08X",
				priority.HighPriority, priority.LowPriority)
		}

		union := priority.HighPriority | priority.LowPriority
		if union != readyBitmap {
			t.Errorf("Union should equal ready bitmap: U=0x%08X R=0x%08X", union, readyBitmap)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 5. CYCLE 1: ISSUE SELECTION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// SelectIssueBundle picks up to 16 instructions to execute:
//   1. If any high priority ops exist, select from high priority only
//   2. Otherwise, select from low priority
//   3. Within selected tier, pick oldest first (highest slot index)
//   4. Skip ops whose dest is already claimed this cycle (WAW within bundle)
//
// Hardware: 32-bit OR tree (tier selection) + parallel CLZ encoder + claimed tracking
// Timing: 150ps (OR tree + encoder + claim check)
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestIssueBundle_Empty(t *testing.T) {
	// WHAT: No ready ops → empty bundle
	// WHY: Nothing to issue
	// HARDWARE: Both tiers empty, valid mask = 0

	priority := PriorityClass{HighPriority: 0, LowPriority: 0}
	window := &InstructionWindow{}

	bundle := SelectIssueBundle(priority, window)

	if bundle.Valid != 0 {
		t.Errorf("Empty priority should produce empty bundle, got valid 0x%04X", bundle.Valid)
	}
}

func TestIssueBundle_SingleOp(t *testing.T) {
	// WHAT: One op available
	// WHY: Minimum positive case
	// HARDWARE: One encoder output valid

	window := &InstructionWindow{}
	window.Ops[0] = Operation{Valid: true, Dest: 10}

	priority := PriorityClass{HighPriority: 0b1, LowPriority: 0}

	bundle := SelectIssueBundle(priority, window)

	if bundle.Valid != 0b1 {
		t.Errorf("Single op should give valid 0x1, got 0x%04X", bundle.Valid)
	}
	if bundle.Indices[0] != 0 {
		t.Errorf("Single op at slot 0, got index %d", bundle.Indices[0])
	}
}

func TestIssueBundle_HighPriorityFirst(t *testing.T) {
	// WHAT: High priority ops selected before low priority
	// WHY: Critical path scheduling - unblock dependent work first
	// HARDWARE: Tier selection MUX chooses high when available

	window := &InstructionWindow{}
	for i := 0; i < 5; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(10 + i)}
	}

	priority := PriorityClass{
		HighPriority: 0b00011, // Ops 0, 1
		LowPriority:  0b11100, // Ops 2, 3, 4
	}

	bundle := SelectIssueBundle(priority, window)

	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		idx := bundle.Indices[i]
		if idx != 0 && idx != 1 {
			t.Errorf("Should only select high priority ops 0,1, got %d", idx)
		}
	}

	count := bits.OnesCount16(bundle.Valid)
	if count != 2 {
		t.Errorf("Should select 2 ops, got %d", count)
	}
}

func TestIssueBundle_LowPriorityWhenNoHigh(t *testing.T) {
	// WHAT: Low priority selected when no high priority available
	// WHY: Don't leave execution units idle
	// HARDWARE: Tier MUX selects low when high is empty

	window := &InstructionWindow{}
	for i := 0; i < 3; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(10 + i)}
	}

	priority := PriorityClass{
		HighPriority: 0,
		LowPriority:  0b111,
	}

	bundle := SelectIssueBundle(priority, window)

	count := bits.OnesCount16(bundle.Valid)
	if count != 3 {
		t.Errorf("Should select 3 low priority ops, got %d", count)
	}
}

func TestIssueBundle_OldestFirst(t *testing.T) {
	// WHAT: Within a tier, select oldest ops first
	// WHY: Older ops have been waiting longer, likely more critical
	// HARDWARE: CLZ finds highest bit (highest slot = oldest)
	//
	// SLOT INDEX = AGE:
	//   Slot 31 = oldest (entered window first)
	//   Slot 0 = newest (entered window last)

	window := &InstructionWindow{}
	for i := 4; i < 8; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(10 + i)}
	}

	priority := PriorityClass{
		HighPriority: 0b11110000, // Ops 4,5,6,7
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	if bundle.Indices[0] != 7 {
		t.Errorf("Oldest op (7) should be first, got %d", bundle.Indices[0])
	}

	prev := bundle.Indices[0]
	for i := 1; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		if bundle.Indices[i] > prev {
			t.Errorf("Should be descending order, %d > %d at position %d", bundle.Indices[i], prev, i)
		}
		prev = bundle.Indices[i]
	}
}

func TestIssueBundle_Exactly16(t *testing.T) {
	// WHAT: Exactly 16 ops available
	// WHY: Perfect match for execution width
	// HARDWARE: All 16 encoder outputs valid

	window := &InstructionWindow{}
	for i := 0; i < 16; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(10 + i)}
	}

	priority := PriorityClass{
		HighPriority: 0xFFFF, // 16 ops
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	if bundle.Valid != 0xFFFF {
		t.Errorf("16 ops should fill bundle, got valid 0x%04X", bundle.Valid)
	}
}

func TestIssueBundle_MoreThan16(t *testing.T) {
	// WHAT: More than 16 ops available
	// WHY: Can only issue 16 per cycle (execution unit limit)
	// HARDWARE: Encoder saturates at 16

	window := &InstructionWindow{}
	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(i)} // Different dests!
	}

	priority := PriorityClass{
		HighPriority: 0xFFFFFFFF, // 32 ops
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	count := bits.OnesCount16(bundle.Valid)
	if count != 16 {
		t.Errorf("Should select exactly 16 ops, got %d", count)
	}

	// Should be oldest 16 (slots 16-31)
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		idx := bundle.Indices[i]
		if idx < 16 {
			t.Errorf("Should select oldest 16 (slots 16-31), got slot %d", idx)
		}
	}
}

func TestIssueBundle_WAWConflict_SameCycle(t *testing.T) {
	// WHAT: Two ops writing same register in same cycle
	// WHY: This is the WAW-within-bundle conflict we must prevent
	// HARDWARE: claimed mask tracks destinations during selection
	//
	// SCENARIO:
	//   Slot 15: writes R10
	//   Slot 10: writes R10
	//   Both are ready. Without claimed tracking, both would issue.
	//   With claimed tracking, only oldest (slot 15) issues this cycle.

	window := &InstructionWindow{}
	window.Ops[15] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[10] = Operation{Valid: true, Src1: 3, Src2: 4, Dest: 10} // Same dest!

	priority := PriorityClass{
		HighPriority: (1 << 15) | (1 << 10), // Both ready
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	count := bits.OnesCount16(bundle.Valid)
	if count != 1 {
		t.Errorf("Should select only 1 op (WAW conflict), got %d", count)
	}

	// Should be the older one (slot 15)
	if bundle.Indices[0] != 15 {
		t.Errorf("Should select older slot 15, got slot %d", bundle.Indices[0])
	}
}

func TestIssueBundle_WAWConflict_Multiple(t *testing.T) {
	// WHAT: Multiple groups of WAW conflicts
	// WHY: Verify claimed tracking handles multiple conflicts
	// HARDWARE: 64-bit claimed mask tracks all 64 possible registers

	window := &InstructionWindow{}
	// Group 1: slots 31, 21, 11 all write R10
	window.Ops[31] = Operation{Valid: true, Dest: 10}
	window.Ops[21] = Operation{Valid: true, Dest: 10}
	window.Ops[11] = Operation{Valid: true, Dest: 10}
	// Group 2: slots 30, 20 write R20
	window.Ops[30] = Operation{Valid: true, Dest: 20}
	window.Ops[20] = Operation{Valid: true, Dest: 20}
	// Independent: slot 25 writes R30
	window.Ops[25] = Operation{Valid: true, Dest: 30}

	priority := PriorityClass{
		HighPriority: (1 << 31) | (1 << 30) | (1 << 25) | (1 << 21) | (1 << 20) | (1 << 11),
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	count := bits.OnesCount16(bundle.Valid)
	if count != 3 {
		t.Errorf("Should select 3 ops (one per dest), got %d", count)
	}

	// Should select: slot 31 (R10), slot 30 (R20), slot 25 (R30)
	selectedDests := make(map[uint8]bool)
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		dest := window.Ops[bundle.Indices[i]].Dest
		if selectedDests[dest] {
			t.Errorf("Selected two ops writing same dest %d", dest)
		}
		selectedDests[dest] = true
	}
}

func TestIssueBundle_WAWConflict_AllSameDest(t *testing.T) {
	// WHAT: All ready ops write same register
	// WHY: Extreme case - only one can issue
	// HARDWARE: claimed mask blocks all but first

	window := &InstructionWindow{}
	for i := 0; i < 8; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: 5} // All write R5
	}

	priority := PriorityClass{
		HighPriority: 0xFF, // All 8 ready
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	count := bits.OnesCount16(bundle.Valid)
	if count != 1 {
		t.Errorf("Should select only 1 op (all write same dest), got %d", count)
	}

	// Should be oldest (slot 7)
	if bundle.Indices[0] != 7 {
		t.Errorf("Should select oldest slot 7, got slot %d", bundle.Indices[0])
	}
}

func TestIssueBundle_NoDuplicates(t *testing.T) {
	// WHAT: Each selected index appears exactly once
	// WHY: Double-issue would be catastrophic
	// HARDWARE: Each CLZ iteration masks out selected bit

	window := &InstructionWindow{}
	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(i)}
	}

	priority := PriorityClass{
		HighPriority: 0xFFFFFFFF,
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	seen := make(map[uint8]int)
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		idx := bundle.Indices[i]
		seen[idx]++
		if seen[idx] > 1 {
			t.Errorf("Index %d selected multiple times", idx)
		}
	}
}

func TestIssueBundle_ValidMaskMatchesCount(t *testing.T) {
	// WHAT: Valid mask bit count equals number of selected ops
	// WHY: Consistency check
	// HARDWARE: Valid mask generated alongside indices

	window := &InstructionWindow{}
	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(i)}
	}

	testCases := []uint32{0, 0b1, 0b11, 0xFF, 0xFFFF, 0xFFFFFFFF}

	for _, bitmap := range testCases {
		priority := PriorityClass{HighPriority: bitmap, LowPriority: 0}
		bundle := SelectIssueBundle(priority, window)

		expected := bits.OnesCount32(bitmap)
		if expected > 16 {
			expected = 16
		}

		got := bits.OnesCount16(bundle.Valid)
		if got != expected {
			t.Errorf("Bitmap 0x%08X: expected %d valid, got %d", bitmap, expected, got)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 6. CYCLE 1: SCOREBOARD UPDATES
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// When instructions issue, their destination registers become pending.
// When instructions complete, their destination registers become ready.
//
// UpdateScoreboardAfterIssue: Called after SelectIssueBundle
//   - Marks dest registers pending (blocks RAW dependent ops)
//   - Sets Issued flag (prevents double-issue, enables producer check to pass)
//
// UpdateScoreboardAfterComplete: Called when execution units signal done
//   - Marks dest registers ready (unblocks dependent ops AND WAW ops)
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestIssueUpdate_Single(t *testing.T) {
	// WHAT: Single op issued, dest becomes pending
	// WHY: Basic scoreboard update
	// HARDWARE: One MarkPending, one Issued flag set

	var sb Scoreboard = ^Scoreboard(0) // All ready
	window := &InstructionWindow{}

	window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	bundle := IssueBundle{
		Indices: [16]uint8{0},
		Valid:   0b1,
	}

	UpdateScoreboardAfterIssue(&sb, window, bundle)

	if sb.IsReady(10) {
		t.Error("Dest register should be pending after issue")
	}
	if !window.Ops[0].Issued {
		t.Error("Issued flag should be set")
	}
}

func TestIssueUpdate_Multiple(t *testing.T) {
	// WHAT: Multiple ops issued in parallel
	// WHY: Typical case - up to 16 ops per cycle
	// HARDWARE: 16 parallel MarkPending operations

	var sb Scoreboard = ^Scoreboard(0)
	window := &InstructionWindow{}

	for i := 0; i < 5; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(10 + i)}
	}

	bundle := IssueBundle{
		Indices: [16]uint8{0, 1, 2, 3, 4},
		Valid:   0b11111,
	}

	UpdateScoreboardAfterIssue(&sb, window, bundle)

	for i := 0; i < 5; i++ {
		if sb.IsReady(uint8(10 + i)) {
			t.Errorf("Register %d should be pending", 10+i)
		}
		if !window.Ops[i].Issued {
			t.Errorf("Op %d should be marked Issued", i)
		}
	}
}

func TestIssueUpdate_EmptyBundle(t *testing.T) {
	// WHAT: Empty bundle doesn't change state
	// WHY: No ops issued, no state change
	// HARDWARE: Valid mask gates all updates

	var sb Scoreboard = ^Scoreboard(0)
	window := &InstructionWindow{}

	window.Ops[0] = Operation{Valid: true, Dest: 10}

	bundle := IssueBundle{Valid: 0}

	UpdateScoreboardAfterIssue(&sb, window, bundle)

	if !sb.IsReady(10) {
		t.Error("Empty bundle should not modify scoreboard")
	}
	if window.Ops[0].Issued {
		t.Error("Empty bundle should not set Issued flag")
	}
}

func TestIssueUpdate_ScatteredSlots(t *testing.T) {
	// WHAT: Issue from non-contiguous window slots
	// WHY: Validates per-slot handling
	// HARDWARE: Each slot in separate SRAM bank

	var sb Scoreboard = ^Scoreboard(0)
	window := &InstructionWindow{}

	slots := []int{0, 7, 15, 22, 31}
	var bundle IssueBundle
	for i, slot := range slots {
		window.Ops[slot] = Operation{Valid: true, Dest: uint8(slot + 10)}
		bundle.Indices[i] = uint8(slot)
		bundle.Valid |= 1 << i
	}

	UpdateScoreboardAfterIssue(&sb, window, bundle)

	for _, slot := range slots {
		if sb.IsReady(uint8(slot + 10)) {
			t.Errorf("Register %d should be pending", slot+10)
		}
		if !window.Ops[slot].Issued {
			t.Errorf("Op at slot %d should be Issued", slot)
		}
	}
}

func TestCompleteUpdate_Single(t *testing.T) {
	// WHAT: Single op completes, dest becomes ready
	// WHY: Basic completion handling
	// HARDWARE: One MarkReady

	var sb Scoreboard // All pending

	destRegs := [16]uint8{10}
	completeMask := uint16(0b1)

	UpdateScoreboardAfterComplete(&sb, destRegs, completeMask)

	if !sb.IsReady(10) {
		t.Error("Register 10 should be ready after completion")
	}
}

func TestCompleteUpdate_Multiple(t *testing.T) {
	// WHAT: Multiple ops complete in parallel
	// WHY: Typical case - variable latency ops complete together
	// HARDWARE: 16 parallel MarkReady operations

	var sb Scoreboard

	destRegs := [16]uint8{10, 11, 12, 13, 14}
	completeMask := uint16(0b11111)

	UpdateScoreboardAfterComplete(&sb, destRegs, completeMask)

	for i := 0; i < 5; i++ {
		if !sb.IsReady(uint8(10 + i)) {
			t.Errorf("Register %d should be ready", 10+i)
		}
	}
}

func TestCompleteUpdate_Selective(t *testing.T) {
	// WHAT: Only some bundle positions complete
	// WHY: Variable latency - MUL takes 2 cycles, ADD takes 1
	// HARDWARE: completeMask gates which updates happen

	var sb Scoreboard

	destRegs := [16]uint8{10, 11, 12, 13}
	completeMask := uint16(0b1010) // Only indices 1 and 3

	UpdateScoreboardAfterComplete(&sb, destRegs, completeMask)

	if !sb.IsReady(11) {
		t.Error("Register 11 (index 1) should be ready")
	}
	if !sb.IsReady(13) {
		t.Error("Register 13 (index 3) should be ready")
	}

	if sb.IsReady(10) {
		t.Error("Register 10 (index 0) should not be ready")
	}
	if sb.IsReady(12) {
		t.Error("Register 12 (index 2) should not be ready")
	}
}

func TestCompleteUpdate_All16(t *testing.T) {
	// WHAT: All 16 positions complete
	// WHY: Maximum throughput case
	// HARDWARE: All 16 MarkReady active

	var sb Scoreboard

	var destRegs [16]uint8
	for i := 0; i < 16; i++ {
		destRegs[i] = uint8(10 + i)
	}
	completeMask := uint16(0xFFFF)

	UpdateScoreboardAfterComplete(&sb, destRegs, completeMask)

	for i := 0; i < 16; i++ {
		if !sb.IsReady(uint8(10 + i)) {
			t.Errorf("Register %d should be ready", 10+i)
		}
	}
}

func TestIssueComplete_Cycle(t *testing.T) {
	// WHAT: Issue then complete - full lifecycle
	// WHY: End-to-end register state tracking
	// HARDWARE: Issue → execution → complete pipeline

	var sb Scoreboard = ^Scoreboard(0)
	window := &InstructionWindow{}

	window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	// Issue
	bundle := IssueBundle{Indices: [16]uint8{0}, Valid: 0b1}
	UpdateScoreboardAfterIssue(&sb, window, bundle)

	if sb.IsReady(10) {
		t.Error("After issue, dest should be pending")
	}

	// Complete
	UpdateScoreboardAfterComplete(&sb, [16]uint8{10}, 0b1)

	if !sb.IsReady(10) {
		t.Error("After complete, dest should be ready")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 7. PIPELINE INTEGRATION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// The scheduler is a 2-stage pipeline:
//   Cycle N:   ScheduleCycle0() computes priority → stored in PipelinedPriority
//   Cycle N+1: ScheduleCycle1() uses PipelinedPriority → returns bundle
//
// In steady state, both cycles run every clock:
//   - Cycle 0 analyzes current window state
//   - Cycle 1 issues based on PREVIOUS cycle's analysis
//
// This overlap is critical for achieving 2-cycle latency at high frequency.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestNewOoOScheduler(t *testing.T) {
	// WHAT: Constructor initializes scoreboard to all-ready
	// WHY: Fresh context has no in-flight instructions
	// HARDWARE: All scoreboard bits set on context switch-in

	sched := NewOoOScheduler()

	for i := uint8(0); i < 64; i++ {
		if !sched.Scoreboard.IsReady(i) {
			t.Errorf("Register %d should be ready after NewOoOScheduler", i)
		}
	}

	if sched.Scoreboard != ^Scoreboard(0) {
		t.Errorf("Scoreboard should be 0xFFFFFFFFFFFFFFFF, got 0x%016X", sched.Scoreboard)
	}
}

func TestPipeline_BasicOperation(t *testing.T) {
	// WHAT: Cycle 0 computes priority, Cycle 1 uses it
	// WHY: Verify pipeline register transfers state
	// HARDWARE: D flip-flops capture priority at cycle boundary

	sched := NewOoOScheduler()

	sched.Window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	sched.ScheduleCycle0()

	if sched.PipelinedPriority.HighPriority == 0 && sched.PipelinedPriority.LowPriority == 0 {
		t.Error("PipelinedPriority should be populated after Cycle 0")
	}

	bundle := sched.ScheduleCycle1()

	if bundle.Valid == 0 {
		t.Error("Cycle 1 should produce bundle from pipelined priority")
	}
}

func TestPipeline_IndependentOps(t *testing.T) {
	// WHAT: 20 independent ops, issued in two batches
	// WHY: Maximum parallelism - all ops ready immediately
	// HARDWARE: Full execution width utilized

	sched := NewOoOScheduler()

	for i := 0; i < 20; i++ {
		sched.Window.Ops[i] = Operation{
			Valid: true,
			Src1:  1,
			Src2:  2,
			Dest:  uint8(10 + i), // Different dests
		}
	}

	// First issue
	sched.ScheduleCycle0()
	bundle1 := sched.ScheduleCycle1()

	count1 := bits.OnesCount16(bundle1.Valid)
	if count1 != 16 {
		t.Errorf("First issue should select 16 ops, got %d", count1)
	}

	// Second issue (4 remaining)
	sched.ScheduleCycle0()
	bundle2 := sched.ScheduleCycle1()

	count2 := bits.OnesCount16(bundle2.Valid)
	if count2 != 4 {
		t.Errorf("Second issue should select 4 ops, got %d", count2)
	}
}

func TestPipeline_DependencyChain(t *testing.T) {
	// WHAT: Chain A→B→C, issued one at a time
	// WHY: Serialized execution due to dependencies
	// HARDWARE: Ready bitmap changes as completions occur
	//
	// With dependency matrix integration:
	// - B waits for A to ISSUE (producer check)
	// - After A issues, B waits for A to COMPLETE (scoreboard check)

	sched := NewOoOScheduler()

	// Chain: slot 20 → slot 10 → slot 5
	sched.Window.Ops[20] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	sched.Window.Ops[5] = Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12}

	// Issue A only (B and C blocked by unissued producer)
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 20 {
		t.Errorf("Should issue A (slot 20) first, got slot %d", bundle.Indices[0])
	}
	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Error("Should issue only A (B and C blocked by producer check)")
	}

	// Complete A
	sched.ScheduleComplete([16]uint8{10}, 0b1)

	// Issue B
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	foundB := false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 && bundle.Indices[i] == 10 {
			foundB = true
		}
	}
	if !foundB {
		t.Error("Should issue B after A completes")
	}

	// Complete B
	sched.ScheduleComplete([16]uint8{11}, 0b1)

	// Issue C
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	foundC := false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 && bundle.Indices[i] == 5 {
			foundC = true
		}
	}
	if !foundC {
		t.Error("Should issue C after B completes")
	}
}

func TestPipeline_Diamond(t *testing.T) {
	// WHAT: Diamond A→{B,C}→D, validates parallel issue
	// WHY: Tests ILP extraction - B and C can run in parallel
	// HARDWARE: Multiple ready ops issued simultaneously

	sched := NewOoOScheduler()

	sched.Window.Ops[25] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.Window.Ops[20] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	sched.Window.Ops[15] = Operation{Valid: true, Src1: 10, Src2: 4, Dest: 12}
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 11, Src2: 12, Dest: 13}

	// Issue A
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 25 {
		t.Errorf("Should issue A first, got slot %d", bundle.Indices[0])
	}

	// Complete A
	sched.ScheduleComplete([16]uint8{10}, 0b1)

	// Issue B and C (parallel)
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	foundB, foundC := false, false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		switch bundle.Indices[i] {
		case 20:
			foundB = true
		case 15:
			foundC = true
		}
	}

	if !foundB || !foundC {
		t.Error("Should issue both B and C in parallel after A completes")
	}

	// Complete B and C
	sched.ScheduleComplete([16]uint8{11, 12}, 0b11)

	// Issue D
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	foundD := false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 && bundle.Indices[i] == 10 {
			foundD = true
		}
	}
	if !foundD {
		t.Error("Should issue D after B and C complete")
	}
}

func TestPipeline_EmptyWindow(t *testing.T) {
	// WHAT: Empty window produces empty bundle
	// WHY: Nothing to schedule
	// HARDWARE: All valid bits 0

	sched := NewOoOScheduler()

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Errorf("Empty window should produce empty bundle, got 0x%04X", bundle.Valid)
	}
}

func TestPipeline_AllBlocked(t *testing.T) {
	// WHAT: All ops blocked on dependencies (external registers)
	// WHY: No forward progress possible
	// HARDWARE: Ready bitmap is 0

	sched := NewOoOScheduler()

	// All ops read from registers that are pending (not ready)
	for i := 0; i < 10; i++ {
		sched.Window.Ops[i] = Operation{
			Valid: true,
			Src1:  50, // Mark as not ready
			Src2:  51, // Mark as not ready
			Dest:  uint8(10 + i),
		}
	}
	sched.Scoreboard.MarkPending(50)
	sched.Scoreboard.MarkPending(51)

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Errorf("All blocked should produce empty bundle, got 0x%04X", bundle.Valid)
	}
}

func TestPipeline_WAWSequence(t *testing.T) {
	// WHAT: Two ops writing same register, issued sequentially
	// WHY: Verify WAW handling through scoreboard
	// HARDWARE: scoreboard[dest] blocks younger WAW until older completes
	//
	// SCENARIO:
	//   Slot 20: A: R5 = f(R1, R2)   # older
	//   Slot 10: B: R5 = g(R3, R4)   # younger, same dest
	//
	// Cycle 1: A issues (scoreboard[5] = 0)
	// Cycle 2: B tries to issue, scoreboard[5] = 0 → NOT ready
	// Cycle N: A completes (scoreboard[5] = 1)
	// Cycle N+1: B issues

	sched := NewOoOScheduler()

	sched.Window.Ops[20] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5}
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 3, Src2: 4, Dest: 5}

	// Issue A (older) - B blocked by claimed mask
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Errorf("Should issue only 1 op (WAW claimed), got %d", bits.OnesCount16(bundle.Valid))
	}
	if bundle.Indices[0] != 20 {
		t.Errorf("Should issue older slot 20, got slot %d", bundle.Indices[0])
	}

	// Now scoreboard[5] = 0, B blocked by scoreboard
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Errorf("B should be blocked (WAW via scoreboard), got valid 0x%04X", bundle.Valid)
	}

	// Complete A
	sched.ScheduleComplete([16]uint8{5}, 0b1)

	// Now B can issue
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Indices[0] != 10 {
		t.Errorf("Should issue B (slot 10) after A completes, got slot %d", bundle.Indices[0])
	}
}

func TestPipeline_StateMachine(t *testing.T) {
	// WHAT: Test complete state machine transitions
	// WHY: Document all valid states and transitions
	// HARDWARE: FSM for each instruction slot

	sched := NewOoOScheduler()

	// State 1: Invalid (empty slot)
	if sched.Window.Ops[0].Valid {
		t.Error("Initial state should be Invalid")
	}

	// State 2: Valid, blocked by producer check (intra-window dependency)
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.Window.Ops[0] = Operation{Valid: true, Src1: 10, Src2: 11, Dest: 12}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	// Only slot 10 should issue (slot 0 blocked by producer)
	if bits.OnesCount16(bundle.Valid) != 1 || bundle.Indices[0] != 10 {
		t.Error("Only producer should issue first")
	}

	// State 3: Valid, blocked by scoreboard (producer issued but not complete)
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Error("Consumer should be blocked by scoreboard")
	}

	// State 4: Valid, ready (producer complete)
	sched.ScheduleComplete([16]uint8{10}, 0b1)
	sched.Scoreboard.MarkReady(11) // Mark other source ready

	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid == 0 {
		t.Error("Consumer should issue after producer completes")
	}

	// State 5: Issued
	if !sched.Window.Ops[0].Issued {
		t.Error("Op should be marked Issued")
	}

	// State 6: Completed (back to ready register)
	sched.ScheduleComplete([16]uint8{12}, 0b1)
	if !sched.Scoreboard.IsReady(12) {
		t.Error("Dest should be ready after completion")
	}

	t.Log("✓ All state transitions verified")
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 8. DEPENDENCY PATTERNS
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// Real programs exhibit various dependency patterns.
// Testing these patterns validates scheduler correctness.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestPattern_Forest(t *testing.T) {
	// WHAT: Multiple independent trees
	// WHY: Maximum ILP - trees can execute in parallel
	//
	// STRUCTURE:
	//   Tree 1: A1 → B1    (slots 31, 28)
	//   Tree 2: A2 → B2    (slots 25, 22)
	//   Tree 3: A3 → B3    (slots 19, 16)

	sched := NewOoOScheduler()

	sched.Window.Ops[31] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.Window.Ops[28] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}

	sched.Window.Ops[25] = Operation{Valid: true, Src1: 4, Src2: 5, Dest: 20}
	sched.Window.Ops[22] = Operation{Valid: true, Src1: 20, Src2: 6, Dest: 21}

	sched.Window.Ops[19] = Operation{Valid: true, Src1: 7, Src2: 8, Dest: 30}
	sched.Window.Ops[16] = Operation{Valid: true, Src1: 30, Src2: 9, Dest: 31}

	// Should issue all roots in parallel (A1, A2, A3)
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	count := bits.OnesCount16(bundle.Valid)
	if count != 3 {
		t.Errorf("Should issue 3 root nodes, got %d", count)
	}
}

func TestPattern_WideTree(t *testing.T) {
	// WHAT: One root, many leaves
	// WHY: Single producer, multiple consumers
	//
	// STRUCTURE:
	//     Root (slot 31)
	//    /|\ ... \
	//   L0 L1 ... L15 (slots 15-0)

	sched := NewOoOScheduler()

	sched.Window.Ops[31] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	for i := 0; i < 16; i++ {
		sched.Window.Ops[i] = Operation{
			Valid: true,
			Src1:  10, // Depends on root
			Src2:  3,
			Dest:  uint8(20 + i),
		}
	}

	// First issue: only root
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 || bundle.Indices[0] != 31 {
		t.Error("Should issue only root first")
	}

	// Complete root
	sched.ScheduleComplete([16]uint8{10}, 0b1)

	// Second issue: all 16 leaves
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 16 {
		t.Errorf("Should issue all 16 leaves, got %d", bits.OnesCount16(bundle.Valid))
	}
}

func TestPattern_DeepChain(t *testing.T) {
	// WHAT: Long serialized dependency chain
	// WHY: Worst case for ILP - minimum parallelism
	//
	// STRUCTURE: Op31 → Op30 → Op29 → ... → Op12 (20 ops)

	sched := NewOoOScheduler()

	for i := 0; i < 20; i++ {
		slot := 31 - i
		var src1 uint8
		if i == 0 {
			src1 = 1
		} else {
			src1 = uint8(9 + i)
		}
		sched.Window.Ops[slot] = Operation{
			Valid: true,
			Src1:  src1,
			Src2:  2,
			Dest:  uint8(10 + i),
		}
	}

	for step := 0; step < 20; step++ {
		expectedSlot := 31 - step

		sched.ScheduleCycle0()
		bundle := sched.ScheduleCycle1()

		if bits.OnesCount16(bundle.Valid) != 1 {
			t.Errorf("Step %d: should issue exactly 1 op, got %d",
				step, bits.OnesCount16(bundle.Valid))
		}

		if bundle.Indices[0] != uint8(expectedSlot) {
			t.Errorf("Step %d: expected slot %d, got %d",
				step, expectedSlot, bundle.Indices[0])
		}

		dest := sched.Window.Ops[expectedSlot].Dest
		sched.ScheduleComplete([16]uint8{dest}, 0b1)
	}
}

func TestPattern_Reduction(t *testing.T) {
	// WHAT: Tree reduction pattern (parallel → serial)
	// WHY: Common in vector operations, sum reductions
	//
	// STRUCTURE:
	//   Level 0: A, B, C, D (independent)
	//   Level 1: E=A+B, F=C+D
	//   Level 2: G=E+F

	sched := NewOoOScheduler()

	sched.Window.Ops[31] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.Window.Ops[30] = Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11}
	sched.Window.Ops[29] = Operation{Valid: true, Src1: 5, Src2: 6, Dest: 12}
	sched.Window.Ops[28] = Operation{Valid: true, Src1: 7, Src2: 8, Dest: 13}

	sched.Window.Ops[27] = Operation{Valid: true, Src1: 10, Src2: 11, Dest: 14}
	sched.Window.Ops[26] = Operation{Valid: true, Src1: 12, Src2: 13, Dest: 15}

	sched.Window.Ops[25] = Operation{Valid: true, Src1: 14, Src2: 15, Dest: 16}

	// Level 0: all 4 in parallel
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 4 {
		t.Errorf("Level 0: should issue 4 ops, got %d", bits.OnesCount16(bundle.Valid))
	}

	// Complete level 0
	sched.ScheduleComplete([16]uint8{10, 11, 12, 13}, 0b1111)

	// Level 1: both E and F
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 2 {
		t.Errorf("Level 1: should issue 2 ops, got %d", bits.OnesCount16(bundle.Valid))
	}

	// Complete level 1
	sched.ScheduleComplete([16]uint8{14, 15}, 0b11)

	// Level 2: G
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 || bundle.Indices[0] != 25 {
		t.Error("Level 2: should issue G")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 9. HAZARD HANDLING
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// CPU hazards are situations where naive execution would produce wrong results.
// SUPRAX handles all three hazard types with minimal hardware:
//
//   RAW: Dependency matrix (producer must issue) + Scoreboard (producer must complete)
//   WAR: Implicit in slot ordering (older reads before younger writes)
//   WAW: scoreboard[dest] check + claimed mask in SelectIssueBundle
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestHazard_RAW_Detected(t *testing.T) {
	// WHAT: Read-After-Write creates dependency
	// WHY: This is the fundamental dependency we track
	//
	// EXAMPLE:
	//   Slot 10: R5 = R1 + R2  (writes R5)
	//   Slot 5:  R6 = R5 + R3  (reads R5 - RAW!)

	window := &InstructionWindow{}

	window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5}
	window.Ops[5] = Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6}

	matrix := BuildDependencyMatrix(window)

	if (matrix[10]>>5)&1 != 1 {
		t.Error("RAW hazard not detected")
	}
}

func TestHazard_WAR_NotTracked(t *testing.T) {
	// WHAT: Write-After-Read is NOT a true dependency
	// WHY: Reader already captured value before writer updates
	//
	// EXAMPLE:
	//   Slot 10: R6 = R5 + R3  (reads R5)
	//   Slot 5:  R5 = R1 + R2  (writes R5 - WAR, not tracked)

	window := &InstructionWindow{}

	window.Ops[10] = Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6}
	window.Ops[5] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5}

	matrix := BuildDependencyMatrix(window)

	if matrix[10] != 0 {
		t.Errorf("WAR should not create dependency, matrix[10]=0x%08X", matrix[10])
	}
	if matrix[5] != 0 {
		t.Errorf("WAR reverse should not create dependency, matrix[5]=0x%08X", matrix[5])
	}
}

func TestHazard_WAW_BlockedByScoreboard(t *testing.T) {
	// WHAT: Write-After-Write blocked by scoreboard[dest] check
	// WHY: Younger writer must wait for older writer to complete
	//
	// EXAMPLE:
	//   Slot 10: R5 = R1 + R2  (writes R5, older)
	//   Slot 5:  R5 = R3 + R4  (writes R5, younger - blocked!)

	sched := NewOoOScheduler()

	sched.Window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5}
	sched.Window.Ops[5] = Operation{Valid: true, Src1: 3, Src2: 4, Dest: 5}

	// First cycle: claimed mask prevents same-cycle WAW
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Errorf("Should issue only 1 (claimed mask), got %d", bits.OnesCount16(bundle.Valid))
	}
	if bundle.Indices[0] != 10 {
		t.Errorf("Should issue older slot 10, got %d", bundle.Indices[0])
	}

	// Scoreboard now has R5 pending
	if sched.Scoreboard.IsReady(5) {
		t.Error("R5 should be pending after older writer issues")
	}

	// Second cycle: younger blocked by scoreboard[dest]=0
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Error("Younger WAW should be blocked by pending dest")
	}

	// Complete older writer
	sched.ScheduleComplete([16]uint8{5}, 0b1)

	// Now younger can issue
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Indices[0] != 5 {
		t.Errorf("Younger should issue after older completes, got slot %d", bundle.Indices[0])
	}
}

func TestHazard_WAW_ClaimedMask(t *testing.T) {
	// WHAT: Two ready ops writing same register in same cycle
	// WHY: claimed mask prevents same-cycle WAW conflict
	//
	// This tests SelectIssueBundle's claimed tracking, not scoreboard.
	// Both ops see scoreboard[dest]=1, but only oldest gets selected.

	window := &InstructionWindow{}
	window.Ops[20] = Operation{Valid: true, Dest: 10}
	window.Ops[15] = Operation{Valid: true, Dest: 10}
	window.Ops[10] = Operation{Valid: true, Dest: 10}

	priority := PriorityClass{
		HighPriority: (1 << 20) | (1 << 15) | (1 << 10),
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Errorf("Should select only 1 (claimed mask), got %d", bits.OnesCount16(bundle.Valid))
	}
	if bundle.Indices[0] != 20 {
		t.Errorf("Should select oldest (slot 20), got slot %d", bundle.Indices[0])
	}
}

func TestHazard_RAW_Chain(t *testing.T) {
	// WHAT: Chain of RAW dependencies
	// WHY: Common pattern - result of one op feeds next

	window := &InstructionWindow{}

	window.Ops[20] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[15] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	window.Ops[10] = Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12}

	matrix := BuildDependencyMatrix(window)

	if (matrix[20]>>15)&1 != 1 {
		t.Error("A→B RAW not detected")
	}
	if (matrix[15]>>10)&1 != 1 {
		t.Error("B→C RAW not detected")
	}
}

func TestHazard_MixedScenario(t *testing.T) {
	// WHAT: Multiple hazard types in same window
	// WHY: Realistic workload
	//
	// Slot 25: R10 = R1 + R2       (producer of R10)
	// Slot 20: R11 = R10 + R3      (RAW: reads R10 from slot 25)
	// Slot 15: R12 = R4 + R5       (independent)
	// Slot 10: R10 = R6 + R7       (WAW with slot 25)
	// Slot 5:  R13 = R10 + R8      (RAW: reads R10)

	window := &InstructionWindow{}

	window.Ops[25] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[20] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	window.Ops[15] = Operation{Valid: true, Src1: 4, Src2: 5, Dest: 12}
	window.Ops[10] = Operation{Valid: true, Src1: 6, Src2: 7, Dest: 10}
	window.Ops[5] = Operation{Valid: true, Src1: 10, Src2: 8, Dest: 13}

	matrix := BuildDependencyMatrix(window)

	// RAW: slot 20 depends on slot 25
	if (matrix[25]>>20)&1 != 1 {
		t.Error("Slot 20 should depend on slot 25")
	}

	// RAW: slot 5 depends on slot 10
	if (matrix[10]>>5)&1 != 1 {
		t.Error("Slot 5 should depend on slot 10")
	}

	// RAW: slot 5 ALSO depends on slot 25
	if (matrix[25]>>5)&1 != 1 {
		t.Error("Slot 5 should ALSO depend on slot 25")
	}

	// Slot 15 is independent
	if matrix[15] != 0 {
		t.Errorf("Slot 15 has no dependents, got matrix[15]=0x%08X", matrix[15])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 10. EDGE CASES
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// Edge cases test boundary conditions and unusual scenarios.
// These often reveal off-by-one errors and implicit assumptions.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestEdge_Register0(t *testing.T) {
	// WHAT: Operations using register 0
	// WHY: Register 0 is LSB, often special-cased in architectures

	sched := NewOoOScheduler()

	sched.Window.Ops[0] = Operation{
		Valid: true,
		Src1:  0,
		Src2:  0,
		Dest:  0,
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 1 {
		t.Error("Op using register 0 should be issuable")
	}
}

func TestEdge_Register63(t *testing.T) {
	// WHAT: Operations using register 63
	// WHY: Register 63 is MSB, boundary condition

	sched := NewOoOScheduler()

	sched.Window.Ops[0] = Operation{
		Valid: true,
		Src1:  63,
		Src2:  63,
		Dest:  63,
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 1 {
		t.Error("Op using register 63 should be issuable")
	}
}

func TestEdge_Slot0(t *testing.T) {
	// WHAT: Op at slot 0 (newest position)
	// WHY: Lowest slot index boundary

	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0)

	window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != 1 {
		t.Errorf("Slot 0 should be in ready bitmap, got 0x%08X", bitmap)
	}
}

func TestEdge_Slot31(t *testing.T) {
	// WHAT: Op at slot 31 (oldest position)
	// WHY: Highest slot index boundary

	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0)

	window.Ops[31] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	depMatrix := BuildDependencyMatrix(window)
	bitmap := ComputeReadyBitmap(window, sb, depMatrix)

	if bitmap != (1 << 31) {
		t.Errorf("Slot 31 should be in ready bitmap, got 0x%08X", bitmap)
	}
}

func TestEdge_Slot31DependsOnSlot0(t *testing.T) {
	// WHAT: Impossible scenario - older can't depend on newer
	// WHY: Validates age check prevents false positives
	//
	// Slot 31 is older (entered first), slot 0 is newer (entered last).
	// Slot 31 cannot possibly depend on slot 0's output.

	window := &InstructionWindow{}

	window.Ops[31] = Operation{Valid: true, Src1: 10, Src2: 2, Dest: 11} // Reads R10
	window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}   // Writes R10

	matrix := BuildDependencyMatrix(window)

	// Slot 31 cannot depend on slot 0 (31 is older)
	if (matrix[0]>>31)&1 != 0 {
		t.Error("Older slot cannot depend on newer slot")
	}
}

func TestEdge_AllSlotsReady(t *testing.T) {
	// WHAT: All 32 slots ready with no dependencies
	// WHY: Maximum parallelism stress test

	sched := NewOoOScheduler()

	for i := 0; i < 32; i++ {
		sched.Window.Ops[i] = Operation{
			Valid: true,
			Src1:  1,
			Src2:  2,
			Dest:  uint8(10 + i),
		}
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	count := bits.OnesCount16(bundle.Valid)
	if count != 16 {
		t.Errorf("Should issue 16 (max), got %d", count)
	}
}

func TestEdge_EmptyAfterClear(t *testing.T) {
	// WHAT: Clear window after filling
	// WHY: Simulates context switch or flush

	sched := NewOoOScheduler()

	for i := 0; i < 10; i++ {
		sched.Window.Ops[i] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)}
	}

	// Clear
	sched.Window = InstructionWindow{}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Error("Cleared window should produce no issues")
	}
}

func TestEdge_SameRegisterAllOps(t *testing.T) {
	// WHAT: All ops read and write same register
	// WHY: Maximum register pressure scenario

	sched := NewOoOScheduler()

	for i := 0; i < 10; i++ {
		sched.Window.Ops[i] = Operation{
			Valid: true,
			Src1:  5,
			Src2:  5,
			Dest:  5,
		}
	}

	// Should issue one (WAW claimed), then serialize
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Errorf("Should issue exactly 1 (WAW claimed), got %d", bits.OnesCount16(bundle.Valid))
	}
}

func TestEdge_ZeroLatency(t *testing.T) {
	// WHAT: Issue and complete in consecutive cycles
	// WHY: Minimum latency path

	sched := NewOoOScheduler()

	sched.Window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	// Issue
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 1 {
		t.Error("Should issue op")
	}

	// Complete immediately
	sched.ScheduleComplete([16]uint8{10}, 0b1)

	if !sched.Scoreboard.IsReady(10) {
		t.Error("Register should be ready after immediate completion")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 11. CORRECTNESS INVARIANTS
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// Invariants are properties that must ALWAYS hold.
// Violating an invariant means the scheduler is fundamentally broken.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestInvariant_NoDoubleIssue(t *testing.T) {
	// INVARIANT: An instruction issues exactly once
	// WHY: Double-issue corrupts architectural state

	sched := NewOoOScheduler()

	sched.Window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	// First issue
	sched.ScheduleCycle0()
	bundle1 := sched.ScheduleCycle1()

	if bundle1.Valid != 1 {
		t.Fatal("First issue should succeed")
	}

	// Second attempt
	sched.ScheduleCycle0()
	bundle2 := sched.ScheduleCycle1()

	if bundle2.Valid != 0 {
		t.Fatal("INVARIANT VIOLATION: Issued instruction selected again!")
	}
}

func TestInvariant_DependenciesRespected(t *testing.T) {
	// INVARIANT: Consumer never issues before producer issues
	// WHY: Would read stale/invalid data

	sched := NewOoOScheduler()

	sched.Window.Ops[20] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	sched.Window.Ops[5] = Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12}

	// First issue - only slot 20 should be possible
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		idx := bundle.Indices[i]
		if idx == 10 || idx == 5 {
			t.Fatalf("INVARIANT VIOLATION: Consumer %d issued before producer!", idx)
		}
	}
}

func TestInvariant_WAWOrdering(t *testing.T) {
	// INVARIANT: WAW writes issue in program order
	// WHY: Final register value must be from last writer

	sched := NewOoOScheduler()

	// Two writers to R5, older at slot 20, newer at slot 10
	sched.Window.Ops[20] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5}
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 3, Src2: 4, Dest: 5}

	// Issue older first
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 20 {
		t.Fatalf("INVARIANT VIOLATION: Newer WAW issued before older!")
	}

	// Younger should be blocked
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Fatalf("INVARIANT VIOLATION: Younger WAW issued while older in flight!")
	}
}

func TestInvariant_PriorityDisjoint(t *testing.T) {
	// INVARIANT: High and low priority sets are disjoint
	// WHY: Each op is exactly one priority

	for trial := 0; trial < 100; trial++ {
		readyBitmap := uint32(trial * 31337)
		var depMatrix DependencyMatrix
		for i := 0; i < 32; i++ {
			depMatrix[i] = uint32((trial + i) * 7)
		}

		priority := ClassifyPriority(readyBitmap, depMatrix)

		if priority.HighPriority&priority.LowPriority != 0 {
			t.Fatalf("INVARIANT VIOLATION: Priority sets overlap!")
		}

		union := priority.HighPriority | priority.LowPriority
		if union != readyBitmap {
			t.Fatalf("INVARIANT VIOLATION: Priority union != ready bitmap!")
		}
	}
}

func TestInvariant_IssuedFlagConsistency(t *testing.T) {
	// INVARIANT: Issued flag set iff instruction was selected for execution
	// WHY: Tracks instruction state machine

	sched := NewOoOScheduler()

	for i := 0; i < 10; i++ {
		sched.Window.Ops[i] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)}
		if sched.Window.Ops[i].Issued {
			t.Fatalf("INVARIANT VIOLATION: Fresh op has Issued=true!")
		}
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		idx := bundle.Indices[i]
		if !sched.Window.Ops[idx].Issued {
			t.Fatalf("INVARIANT VIOLATION: Selected op not marked Issued!")
		}
	}

	for i := 0; i < 32; i++ {
		wasSelected := false
		for j := 0; j < 16; j++ {
			if (bundle.Valid>>j)&1 != 0 && bundle.Indices[j] == uint8(i) {
				wasSelected = true
				break
			}
		}
		if !wasSelected && sched.Window.Ops[i].Issued {
			t.Fatalf("INVARIANT VIOLATION: Unselected op %d has Issued=true!", i)
		}
	}
}

func TestInvariant_ScoreboardConsistency(t *testing.T) {
	// INVARIANT: Scoreboard reflects in-flight status
	// WHY: Scoreboard drives readiness decisions

	sched := NewOoOScheduler()

	// Initially all ready
	for i := uint8(0); i < 64; i++ {
		if !sched.Scoreboard.IsReady(i) {
			t.Fatalf("INVARIANT VIOLATION: Fresh scoreboard has pending register!")
		}
	}

	sched.Window.Ops[0] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Dest should be pending
	if sched.Scoreboard.IsReady(10) {
		t.Fatalf("INVARIANT VIOLATION: Issued dest still marked ready!")
	}

	// Complete
	sched.ScheduleComplete([16]uint8{10}, 0b1)

	// Dest should be ready
	if !sched.Scoreboard.IsReady(10) {
		t.Fatalf("INVARIANT VIOLATION: Completed dest not marked ready!")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 12. STRESS TESTS
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// Stress tests push the scheduler to limits with high-volume operations.
// They help find race conditions and resource exhaustion bugs.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestStress_RepeatedIssueCycles(t *testing.T) {
	// WHAT: Many issue cycles with refilling window
	// WHY: Steady-state behavior

	sched := NewOoOScheduler()
	totalIssued := 0

	for cycle := 0; cycle < 100; cycle++ {
		// Fill empty slots
		for i := 0; i < 32; i++ {
			if !sched.Window.Ops[i].Valid || sched.Window.Ops[i].Issued {
				sched.Window.Ops[i] = Operation{
					Valid: true,
					Src1:  1,
					Src2:  2,
					Dest:  uint8((cycle*32 + i) % 64),
				}
			}
		}

		sched.ScheduleCycle0()
		bundle := sched.ScheduleCycle1()

		count := bits.OnesCount16(bundle.Valid)
		totalIssued += count

		// Complete all issued ops
		var destRegs [16]uint8
		for i := 0; i < 16; i++ {
			if (bundle.Valid>>i)&1 != 0 {
				destRegs[i] = sched.Window.Ops[bundle.Indices[i]].Dest
			}
		}
		sched.ScheduleComplete(destRegs, bundle.Valid)
	}

	if totalIssued < 1000 {
		t.Errorf("Should issue many ops over 100 cycles, got %d", totalIssued)
	}
}

func TestStress_LongChainResolution(t *testing.T) {
	// WHAT: Resolve maximum-length dependency chain
	// WHY: Worst-case serialization

	sched := NewOoOScheduler()

	for i := 0; i < 32; i++ {
		slot := 31 - i
		var src1 uint8
		if i == 0 {
			src1 = 1
		} else {
			src1 = uint8(9 + i)
		}
		sched.Window.Ops[slot] = Operation{
			Valid: true,
			Src1:  src1,
			Src2:  2,
			Dest:  uint8(10 + i),
		}
	}

	for step := 0; step < 32; step++ {
		sched.ScheduleCycle0()
		bundle := sched.ScheduleCycle1()

		if bits.OnesCount16(bundle.Valid) != 1 {
			t.Fatalf("Step %d: should issue exactly 1", step)
		}

		expectedSlot := uint8(31 - step)
		if bundle.Indices[0] != expectedSlot {
			t.Fatalf("Step %d: expected slot %d, got %d", step, expectedSlot, bundle.Indices[0])
		}

		dest := sched.Window.Ops[expectedSlot].Dest
		sched.ScheduleComplete([16]uint8{dest}, 0b1)
	}
}

func TestStress_WAWHeavyWorkload(t *testing.T) {
	// WHAT: Many ops writing same registers
	// WHY: Stress test WAW handling

	sched := NewOoOScheduler()

	// 16 ops all write R5
	for i := 0; i < 16; i++ {
		sched.Window.Ops[31-i] = Operation{
			Valid: true,
			Src1:  uint8(10 + i),
			Src2:  uint8(30 + i),
			Dest:  5,
		}
	}

	// Issue one
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Errorf("Should issue only 1 (WAW claimed), got %d", bits.OnesCount16(bundle.Valid))
	}
	if bundle.Indices[0] != 31 {
		t.Errorf("Should issue oldest (slot 31), got slot %d", bundle.Indices[0])
	}

	// Complete and issue remaining one by one
	for i := 0; i < 16; i++ {
		sched.ScheduleComplete([16]uint8{5}, 0b1)
		sched.ScheduleCycle0()
		bundle = sched.ScheduleCycle1()

		if i < 15 {
			if bits.OnesCount16(bundle.Valid) != 1 {
				t.Errorf("Step %d: should issue 1, got %d", i, bits.OnesCount16(bundle.Valid))
			}
		}
	}

	if !sched.Scoreboard.IsReady(5) {
		t.Error("R5 should be ready after all completions")
	}
}

func TestStress_RapidComplete(t *testing.T) {
	// WHAT: Many completions in rapid succession
	// WHY: Tests scoreboard update throughput

	var sb Scoreboard

	for cycle := 0; cycle < 100; cycle++ {
		var destRegs [16]uint8
		for i := 0; i < 16; i++ {
			destRegs[i] = uint8((cycle*16 + i) % 64)
		}
		UpdateScoreboardAfterComplete(&sb, destRegs, 0xFFFF)
	}

	// After many completions, scoreboard should have many bits set
	readyCount := bits.OnesCount64(uint64(sb))
	if readyCount == 0 {
		t.Error("Scoreboard should have ready registers")
	}
}

func TestStress_MixedWorkload(t *testing.T) {
	// WHAT: Mix of independent ops and dependency chains
	// WHY: Realistic workload pattern
	//
	// PRIORITY BEHAVIOR:
	//   Chain members with dependents → HIGH priority (issue one at a time)
	//   Chain tail + independent ops → LOW priority (all issue together when ready)

	sched := NewOoOScheduler()

	// Independent ops (slots 31-26): all read R1,R2, write different dests
	for i := 0; i < 6; i++ {
		sched.Window.Ops[31-i] = Operation{
			Valid: true,
			Src1:  1,
			Src2:  2,
			Dest:  uint8(10 + i),
		}
	}

	// Chain (slots 25-20): each depends on previous
	// slot 25: src=3 → dest=20
	// slot 24: src=20 → dest=21
	// slot 23: src=21 → dest=22
	// slot 22: src=22 → dest=23
	// slot 21: src=23 → dest=24
	// slot 20: src=24 → dest=25 (chain tail, no dependents)
	for i := 0; i < 6; i++ {
		var src1 uint8
		if i == 0 {
			src1 = 3
		} else {
			src1 = uint8(19 + i)
		}
		sched.Window.Ops[25-i] = Operation{
			Valid: true,
			Src1:  src1,
			Src2:  4,
			Dest:  uint8(20 + i),
		}
	}

	// Track total issued
	totalIssued := 0

	// Issue cycle 1: Only slot 25 (chain head, HIGH priority)
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()
	totalIssued += bits.OnesCount16(bundle.Valid)

	if bits.OnesCount16(bundle.Valid) != 1 || bundle.Indices[0] != 25 {
		t.Errorf("Cycle 1: expected only slot 25, got %d ops", bits.OnesCount16(bundle.Valid))
	}
	sched.ScheduleComplete([16]uint8{20}, 0b1)

	// Issue cycles 2-5: slots 24, 23, 22, 21 (each HIGH priority)
	for expected := uint8(24); expected >= 21; expected-- {
		sched.ScheduleCycle0()
		bundle = sched.ScheduleCycle1()
		totalIssued += bits.OnesCount16(bundle.Valid)

		if bits.OnesCount16(bundle.Valid) != 1 || bundle.Indices[0] != expected {
			t.Errorf("Expected slot %d, got %d ops, first=%d",
				expected, bits.OnesCount16(bundle.Valid), bundle.Indices[0])
		}
		// Complete the issued op
		dest := sched.Window.Ops[expected].Dest
		sched.ScheduleComplete([16]uint8{dest}, 0b1)
	}

	// Issue cycle 6: slot 20 (chain tail) + slots 31-26 (independent)
	// All are LOW priority now, all issue together (7 total)
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()
	totalIssued += bits.OnesCount16(bundle.Valid)

	if bits.OnesCount16(bundle.Valid) != 7 {
		t.Errorf("Final cycle: expected 7 ops (chain tail + 6 independent), got %d",
			bits.OnesCount16(bundle.Valid))
	}

	// Verify total: 1 + 4 + 7 = 12 ops
	if totalIssued != 12 {
		t.Errorf("Total issued should be 12, got %d", totalIssued)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 13. PIPELINE HAZARDS
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// Pipeline hazards occur when state changes between Cycle 0 and Cycle 1.
// The pipelined priority may become stale, but correctness is preserved.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestPipelineHazard_CompletionBetweenCycles(t *testing.T) {
	// WHAT: Completion arrives between Cycle 0 and Cycle 1
	// WHY: Tests pipeline register staleness
	//
	// Cycle 0: Analyze - sees R10 pending
	// Between: R10 completes
	// Cycle 1: Select - uses stale priority (doesn't include newly-ready op)
	//
	// This is SAFE: op just waits one more cycle. Not a bug, just latency.

	sched := NewOoOScheduler()

	// First op writes R10
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	// Issue it
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Second op reads R10
	sched.Window.Ops[5] = Operation{Valid: true, Src1: 10, Src2: 2, Dest: 11}

	// Cycle 0: Op 5 blocked (R10 pending)
	sched.ScheduleCycle0()

	// Completion arrives between cycles
	sched.ScheduleComplete([16]uint8{10}, 0b1)

	// Cycle 1: Uses stale priority (computed when R10 was pending)
	bundle := sched.ScheduleCycle1()

	// Op might not be issued this cycle (stale priority)
	t.Logf("Bundle valid: 0x%04X (may be 0 due to stale priority)", bundle.Valid)

	// Next full cycle should catch it
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid == 0 {
		t.Error("Op should be issued after fresh analysis")
	}
}

func TestPipelineHazard_IssueInvalidatesReady(t *testing.T) {
	// WHAT: Cycle 1 issue changes state seen by next Cycle 0
	// WHY: Verifies pipeline correctly handles state changes

	sched := NewOoOScheduler()

	// Two independent ops
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.Window.Ops[5] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 11}

	// First cycle: both ready
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 2 {
		t.Errorf("Should issue 2 ops, got %d", bits.OnesCount16(bundle.Valid))
	}

	// Second cycle: both issued, none ready
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Errorf("No ops should be ready, got 0x%04X", bundle.Valid)
	}
}

func TestPipelineHazard_WindowModification(t *testing.T) {
	// WHAT: Window changes between Cycle 0 and Cycle 1
	// WHY: Simulates new instructions arriving

	sched := NewOoOScheduler()

	sched.Window.Ops[10] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	// Cycle 0: Analyze with one op
	sched.ScheduleCycle0()

	// New op arrives between cycles
	sched.Window.Ops[5] = Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11}

	// Cycle 1: New op not in priority (computed before arrival)
	bundle := sched.ScheduleCycle1()

	// Only original op should be issued (new op has stale priority)
	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Logf("Got %d ops (new op may or may not be included)", bits.OnesCount16(bundle.Valid))
	}

	// Next cycle should see new op
	sched.ScheduleCycle0()
	// New op now analyzed
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// 14. DOCUMENTATION TESTS
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// These tests document design properties and constraints.
// They also verify assumptions used elsewhere in the codebase.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func TestDoc_StructureSizes(t *testing.T) {
	// WHAT: Document structure sizes for hardware budgeting
	// WHY: RTL needs to know exact bit widths

	t.Logf("Operation size: %d bytes", unsafe.Sizeof(Operation{}))
	t.Logf("InstructionWindow size: %d bytes", unsafe.Sizeof(InstructionWindow{}))
	t.Logf("Scoreboard size: %d bytes", unsafe.Sizeof(Scoreboard(0)))
	t.Logf("DependencyMatrix size: %d bytes", unsafe.Sizeof(DependencyMatrix{}))
	t.Logf("PriorityClass size: %d bytes", unsafe.Sizeof(PriorityClass{}))
	t.Logf("IssueBundle size: %d bytes", unsafe.Sizeof(IssueBundle{}))
	t.Logf("OoOScheduler size: %d bytes", unsafe.Sizeof(OoOScheduler{}))
}

func TestDoc_Constants(t *testing.T) {
	// WHAT: Verify constant relationships
	// WHY: Document design constraints

	if WindowSize != 32 {
		t.Errorf("WindowSize should be 32, got %d", WindowSize)
	}
	if NumRegisters != 64 {
		t.Errorf("NumRegisters should be 64, got %d", NumRegisters)
	}
	if IssueWidth != 16 {
		t.Errorf("IssueWidth should be 16, got %d", IssueWidth)
	}

	// Issue width <= Window size (can't issue more than available)
	if IssueWidth > WindowSize {
		t.Error("IssueWidth cannot exceed WindowSize")
	}

	// Window fits in uint32 bitmap
	if WindowSize > 32 {
		t.Error("WindowSize must fit in uint32 bitmap")
	}

	// Registers fit in uint64 bitmap
	if NumRegisters > 64 {
		t.Error("NumRegisters must fit in uint64 bitmap")
	}
}

func TestDoc_AgingOrder(t *testing.T) {
	// WHAT: Document slot index = age relationship
	// WHY: Critical for understanding dependency/selection logic
	//
	// RULE: Higher slot index = older instruction
	//   Slot 31: Oldest (entered window first)
	//   Slot 0:  Newest (entered window last)

	window := &InstructionWindow{}

	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(i)}
	}

	priority := PriorityClass{LowPriority: 0xFFFFFFFF}
	bundle := SelectIssueBundle(priority, window)

	// First selected should be oldest (slot 31)
	if bundle.Indices[0] != 31 {
		t.Errorf("Oldest-first: expected slot 31 first, got slot %d", bundle.Indices[0])
	}

	t.Log("✓ Slot index = age confirmed: higher slot = older instruction")
}

func TestDoc_HazardSummary(t *testing.T) {
	// WHAT: Document hazard handling summary
	// WHY: Single source of truth for hazard logic

	t.Log("HAZARD HANDLING SUMMARY:")
	t.Log("========================")
	t.Log("")
	t.Log("RAW (Read-After-Write):")
	t.Log("  - Phase 1: Producer must ISSUE (dependency matrix + Issued flag)")
	t.Log("  - Phase 2: Producer must COMPLETE (scoreboard[src] = 1)")
	t.Log("  - Cost: Dependency matrix (already needed) + producer scan")
	t.Log("")
	t.Log("WAR (Write-After-Read):")
	t.Log("  - Implicit: older reads before younger writes")
	t.Log("  - Cost: ZERO (slot index encodes age)")
	t.Log("")
	t.Log("WAW (Write-After-Write):")
	t.Log("  - Phase 1: scoreboard[dest] = 1 (older writer must complete)")
	t.Log("  - Phase 2: claimed mask in SelectIssueBundle (same-cycle WAW)")
	t.Log("  - Cost: 32 AND gates + 64-bit combinational mask")
}

func TestDoc_PipelineTimeline(t *testing.T) {
	// WHAT: Document pipeline execution timeline
	// WHY: Helps understand latency characteristics

	t.Log("PIPELINE TIMELINE:")
	t.Log("==================")
	t.Log("")
	t.Log("Cycle N:")
	t.Log("  Cycle 0: Build dep matrix → Compute ready → Classify priority")
	t.Log("           Store result in PipelinedPriority register")
	t.Log("")
	t.Log("Cycle N+1:")
	t.Log("  Cycle 1: Read PipelinedPriority → Select bundle → Update scoreboard")
	t.Log("           Output: IssueBundle to execution units")
	t.Log("")
	t.Log("Steady State: Both stages overlap")
	t.Log("  Cycle N:   C0 analyzes, C1 issues from N-1 analysis")
	t.Log("  Cycle N+1: C0 analyzes, C1 issues from N analysis")
	t.Log("")
	t.Log("Issue Latency: 2 cycles from window entry to execution")
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// BENCHMARK TESTS
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func BenchmarkBuildDependencyMatrix(b *testing.B) {
	window := &InstructionWindow{}

	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{
			Valid: true,
			Src1:  uint8(i % 64),
			Src2:  uint8((i + 1) % 64),
			Dest:  uint8((i + 2) % 64),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		BuildDependencyMatrix(window)
	}
}

func BenchmarkComputeReadyBitmap(b *testing.B) {
	window := &InstructionWindow{}
	var sb Scoreboard = ^Scoreboard(0)

	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{
			Valid: true,
			Src1:  uint8(i % 64),
			Src2:  uint8((i + 1) % 64),
			Dest:  uint8((i + 2) % 64),
		}
	}

	depMatrix := BuildDependencyMatrix(window)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ComputeReadyBitmap(window, sb, depMatrix)
	}
}

func BenchmarkClassifyPriority(b *testing.B) {
	readyBitmap := uint32(0xFFFFFFFF)
	var depMatrix DependencyMatrix
	for i := 0; i < 32; i++ {
		depMatrix[i] = uint32(i * 31337)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ClassifyPriority(readyBitmap, depMatrix)
	}
}

func BenchmarkSelectIssueBundle(b *testing.B) {
	window := &InstructionWindow{}
	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(i)}
	}

	priority := PriorityClass{
		HighPriority: 0xFFFF0000,
		LowPriority:  0x0000FFFF,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		SelectIssueBundle(priority, window)
	}
}

func BenchmarkFullCycle(b *testing.B) {
	sched := NewOoOScheduler()

	for i := 0; i < 32; i++ {
		sched.Window.Ops[i] = Operation{
			Valid: true,
			Src1:  uint8(i % 64),
			Src2:  uint8((i + 1) % 64),
			Dest:  uint8((i + 2) % 64),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sched.ScheduleCycle0()
		bundle := sched.ScheduleCycle1()

		// Reset issued flags for next iteration
		for j := 0; j < 16; j++ {
			if (bundle.Valid>>j)&1 != 0 {
				sched.Window.Ops[bundle.Indices[j]].Issued = false
			}
		}
		sched.Scoreboard = ^Scoreboard(0)
	}
}
