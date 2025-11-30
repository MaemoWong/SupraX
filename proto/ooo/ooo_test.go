package ooo

import (
	"math/bits"
	"testing"
	"unsafe"
)

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// SUPRAX Out-of-Order Scheduler - Test Suite
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
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
// KEY ARCHITECTURAL FEATURES TESTED:
// ──────────────────────────────────
//
// INCREMENTAL DEPENDENCY MATRIX:
//   Dependencies tracked as STATE, not recomputed each cycle.
//   Updated when instructions enter (UpdateDependenciesOnEnter) or
//   retire (UpdateDependenciesOnRetire). This moves computation
//   off the critical scheduling path.
//
// BYPASS FORWARDING:
//   Single-cycle ALU forwarding reduces dependent chain latency.
//   Without bypass: 4 cycles (issue → execute → complete → ready → issue)
//   With bypass: 2 cycles (issue → bypass available → issue)
//   Tracked via LastIssuedDests and LastIssuedValid.
//
// UNISSUED VALID BITMAP:
//   O(1) producer check via bitmap AND + zero-detect.
//   Replaces O(n) OR tree scan over all older slots.
//
// SCOREBOARD:
//   A bitmap tracking which registers contain valid data.
//   Bit N = 1 means register N is "ready" (has valid data).
//   Bit N = 0 means register N is "pending" (being computed).
//
// DEPENDENCY:
//   Instruction B depends on instruction A if B reads a register that A writes.
//   Example: A: R3 = R1 + R2    (writes R3)
//            B: R5 = R3 + R4    (reads R3 - depends on A!)
//   B cannot execute until A completes OR bypass is available.
//
// RAW/WAR/WAW HAZARDS:
//   RAW (Read-After-Write): True dependency. B reads what A writes. Must wait.
//   WAR (Write-After-Read): Anti-dependency. B writes what A reads. Implicit in age.
//   WAW (Write-After-Write): Output dependency. B writes same dest as A.
//        Handled by scoreboard[dest] check + claimed mask in SelectIssueBundle.
//
// SLOT INDEX = AGE:
//   Instructions enter the window in program order.
//   Higher slot index = older instruction = entered earlier.
//   Slot 31 is oldest, Slot 0 is newest.
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// TEST COVERAGE MATRIX
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// This test suite aims for 100% coverage of:
//   - All public functions and methods
//   - All code paths within functions
//   - All boundary conditions (slot 0, slot 31, register 0, register 63)
//   - All hazard types (RAW, WAR, WAW)
//   - All dependency patterns (chains, diamonds, forests, wide trees)
//   - All state transitions (enter → issue → complete → retire)
//   - All bypass scenarios (single, multiple, expiration)
//   - All priority classifications (high only, low only, mixed, empty)
//   - All error conditions and edge cases
//
// COVERAGE CATEGORIES:
//   [UNIT]        Single function/component in isolation
//   [INTEGRATION] Multiple components working together
//   [INVARIANT]   Properties that must ALWAYS hold
//   [STRESS]      High-volume, repeated operations
//   [BOUNDARY]    Edge cases at limits of valid input
//   [PATTERN]     Real-world dependency patterns
//   [HAZARD]      CPU hazard handling verification
//   [LIFECYCLE]   Full instruction lifecycle
//   [PIPELINE]    2-stage pipeline behavior
//   [BYPASS]      Forwarding/bypass network
//   [REGRESSION]  Specific bug scenarios
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// TEST ORGANIZATION
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Tests are organized to mirror the hardware components and pipeline:
//
// 1. SCOREBOARD TESTS
//    Basic register readiness tracking
//
// 2. INCREMENTAL DEPENDENCY MATRIX
//    State-based dependency tracking (enter/retire updates)
//
// 3. UNISSUED VALID BITMAP
//    O(1) producer blocking check
//
// 4. BYPASS FORWARDING
//    Single-cycle ALU forwarding
//
// 5. READY BITMAP
//    Which instructions have all sources ready, dest free, and producers issued?
//
// 6. PRIORITY CLASSIFICATION
//    Which ready instructions are on critical path?
//
// 7. ISSUE SELECTION
//    Pick up to 16 instructions to execute (with WAW conflict avoidance)
//
// 8. STATE UPDATES
//    UpdateAfterIssue, UpdateScoreboardAfterComplete
//
// 9. INSTRUCTION LIFECYCLE
//    EnterInstruction, RetireInstruction
//
// 10. PIPELINE INTEGRATION
//     Full 2-cycle pipeline behavior
//
// 11. DEPENDENCY PATTERNS
//     Chains, diamonds, forests, etc.
//
// 12. HAZARD HANDLING
//     RAW, WAR, WAW scenarios
//
// 13. EDGE CASES
//     Boundary conditions, corner cases
//
// 14. CORRECTNESS INVARIANTS
//     Properties that must ALWAYS hold
//
// 15. STRESS TESTS
//     High-volume, repeated operations
//
// 16. BYPASS INTEGRATION
//     Full bypass forwarding scenarios
//
// 17. STATE CONSISTENCY
//     Verify state remains valid after various operations
//
// 18. REGRESSION TESTS
//     Specific scenarios that could harbor bugs
//
// 19. DOCUMENTATION TESTS
//     Verify assumptions, print specs
//
// 20. BENCHMARKS
//     Performance measurement
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 1. SCOREBOARD TESTS
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// The scoreboard is a 64-bit bitmap tracking register readiness.
// It's the simplest component but fundamental to everything else.
//
// HARDWARE MAPPING:
//   - 64 flip-flops, one per architectural register
//   - Read: 6-to-64 decoder → single bit select
//   - MarkReady: OR gate sets single bit
//   - MarkPending: AND gate clears single bit
//
// SCOREBOARD INVARIANTS:
//   - Bit N = 1 means register N contains valid, committed data
//   - Bit N = 0 means register N is being written by in-flight instruction
//   - All bits start at 1 (all registers valid on context switch-in)
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestScoreboard_InitialState(t *testing.T) {
	// WHAT: Verify scoreboard starts with all registers not ready (zero-initialized)
	// WHY: Zero-initialized state represents "all registers pending"
	// HARDWARE: All flip-flops reset to 0
	// CATEGORY: [UNIT] [BOUNDARY]
	//
	// NOTE: This tests the Go zero-value behavior. In actual use,
	// NewOoOScheduler() sets scoreboard to all 1s (all ready).

	var sb Scoreboard

	// Verify every register reports not ready
	for i := uint8(0); i < 64; i++ {
		if sb.IsReady(i) {
			t.Errorf("Register %d should not be ready on init (scoreboard=0x%016X)", i, sb)
		}
	}

	// Verify raw value is zero
	if sb != 0 {
		t.Errorf("Initial scoreboard should be 0, got 0x%016X", sb)
	}
}

func TestScoreboard_MarkReady_Single(t *testing.T) {
	// WHAT: Mark one register ready, verify only that bit changes
	// WHY: Basic functionality - execution completes, register becomes valid
	// HARDWARE: OR gate sets single bit: scoreboard |= (1 << reg)
	// CATEGORY: [UNIT]

	var sb Scoreboard

	sb.MarkReady(5)

	// Target register should be ready
	if !sb.IsReady(5) {
		t.Error("Register 5 should be ready after MarkReady")
	}

	// Adjacent registers should not be affected (no crosstalk)
	if sb.IsReady(4) {
		t.Error("Register 4 should not be affected")
	}
	if sb.IsReady(6) {
		t.Error("Register 6 should not be affected")
	}

	// Verify exact bit pattern
	expected := Scoreboard(1 << 5)
	if sb != expected {
		t.Errorf("Expected 0x%016X, got 0x%016X", expected, sb)
	}
}

func TestScoreboard_MarkPending_Single(t *testing.T) {
	// WHAT: Mark a ready register as pending
	// WHY: When instruction issues, its destination becomes pending (awaiting result)
	// HARDWARE: AND gate clears single bit: scoreboard &= ~(1 << reg)
	// CATEGORY: [UNIT]

	var sb Scoreboard
	sb.MarkReady(5) // First make it ready

	sb.MarkPending(5)

	// Register should now be pending
	if sb.IsReady(5) {
		t.Error("Register 5 should be pending after MarkPending")
	}

	// Scoreboard should be back to zero
	if sb != 0 {
		t.Errorf("Scoreboard should be 0 after marking only ready register pending, got 0x%016X", sb)
	}
}

func TestScoreboard_Idempotent(t *testing.T) {
	// WHAT: Calling MarkReady/MarkPending multiple times has no extra effect
	// WHY: Hardware naturally idempotent (OR 1 with 1 = 1, AND 0 with 0 = 0)
	// HARDWARE: No special handling needed - bitwise ops are idempotent
	// CATEGORY: [UNIT] [INVARIANT]

	var sb Scoreboard

	// Multiple MarkReady calls
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

	// Multiple MarkPending calls
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
	// CATEGORY: [UNIT] [BOUNDARY]

	var sb Scoreboard

	// Mark all registers ready one by one
	for i := uint8(0); i < 64; i++ {
		sb.MarkReady(i)
	}

	// Verify all are ready
	for i := uint8(0); i < 64; i++ {
		if !sb.IsReady(i) {
			t.Errorf("Register %d should be ready", i)
		}
	}

	// Should be all 1s
	if sb != ^Scoreboard(0) {
		t.Errorf("All registers ready should be 0xFFFFFFFFFFFFFFFF, got 0x%016X", sb)
	}

	// Mark all registers pending one by one
	for i := uint8(0); i < 64; i++ {
		sb.MarkPending(i)
	}

	// Verify all are pending
	for i := uint8(0); i < 64; i++ {
		if sb.IsReady(i) {
			t.Errorf("Register %d should be pending", i)
		}
	}

	// Should be all 0s
	if sb != 0 {
		t.Errorf("All registers pending should be 0, got 0x%016X", sb)
	}
}

func TestScoreboard_BoundaryRegisters(t *testing.T) {
	// WHAT: Test register 0 (LSB) and register 63 (MSB)
	// WHY: Boundary conditions often harbor bugs (off-by-one, sign extension)
	// HARDWARE: Validates bit indexing at extremes
	// CATEGORY: [UNIT] [BOUNDARY]

	var sb Scoreboard

	// Test register 0 (LSB)
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

	// Test register 63 (MSB)
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
	// CATEGORY: [UNIT] [PATTERN]

	var sb Scoreboard

	// Set even registers
	for i := uint8(0); i < 64; i += 2 {
		sb.MarkReady(i)
	}

	// Verify pattern
	for i := uint8(0); i < 64; i++ {
		expected := (i % 2) == 0
		if sb.IsReady(i) != expected {
			t.Errorf("Register %d: expected ready=%v, got ready=%v", i, expected, sb.IsReady(i))
		}
	}

	// Verify exact bit pattern (0x5555... = alternating bits starting with LSB set)
	expected := Scoreboard(0x5555555555555555)
	if sb != expected {
		t.Errorf("Checkerboard pattern wrong, expected 0x%016X, got 0x%016X", expected, sb)
	}
}

func TestScoreboard_IndependentUpdates(t *testing.T) {
	// WHAT: Updates to one register don't affect others
	// WHY: Critical correctness property - false dependencies would break everything
	// HARDWARE: Validates per-bit isolation in flip-flop array
	// CATEGORY: [UNIT] [INVARIANT]

	var sb Scoreboard

	// Set up initial state
	sb.MarkReady(10)
	sb.MarkReady(20)
	sb.MarkReady(30)

	// Modify middle register
	sb.MarkPending(20)

	// Verify other registers unaffected
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

func TestScoreboard_ReverseOrder(t *testing.T) {
	// WHAT: Mark registers in reverse order (63 down to 0)
	// WHY: Verify no order-dependent bugs
	// HARDWARE: Validates that register index decoding works regardless of access pattern
	// CATEGORY: [UNIT]

	var sb Scoreboard

	// Mark in reverse order
	for i := 63; i >= 0; i-- {
		sb.MarkReady(uint8(i))
	}

	// All should be ready
	for i := uint8(0); i < 64; i++ {
		if !sb.IsReady(i) {
			t.Errorf("Register %d should be ready (reverse fill)", i)
		}
	}
}

func TestScoreboard_RandomAccess(t *testing.T) {
	// WHAT: Access registers in non-sequential order
	// WHY: Real workloads access registers pseudo-randomly
	// HARDWARE: Validates decoder handles any valid input
	// CATEGORY: [UNIT]

	var sb Scoreboard

	// Access pattern that exercises various bits
	accessOrder := []uint8{37, 2, 58, 15, 41, 8, 63, 0, 29, 50}

	for _, reg := range accessOrder {
		sb.MarkReady(reg)
	}

	for _, reg := range accessOrder {
		if !sb.IsReady(reg) {
			t.Errorf("Register %d should be ready after random access", reg)
		}
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 2. INCREMENTAL DEPENDENCY MATRIX
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// The dependency matrix tracks producer→consumer relationships within the window.
// INCREMENTAL DESIGN: Matrix is STATE, updated when instructions enter/exit.
// This moves computation off the critical scheduling path.
//
// matrix[i] bit j = 1 means "instruction at slot j depends on instruction at slot i"
//
// A dependency exists when:
//   1. Slot i writes a register that slot j reads (RAW hazard)
//   2. Slot i is older than slot j (i > j, since higher slot = older)
//
// The slot index check is CRITICAL:
//   - Higher slot = older instruction (entered window earlier)
//   - Producer must be older to create valid dependency
//   - This prevents false WAR dependencies
//
// HARDWARE MAPPING:
//   - 32×32 = 1024 flip-flops storing dependency state
//   - Update logic: 6-bit comparators for register matching
//   - Read: Direct wire fan-out (very fast)
//   - Update: Triggered by dispatch/retire (off critical path)
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestDepMatrix_InitialEmpty(t *testing.T) {
	// WHAT: Fresh scheduler has empty dependency matrix
	// WHY: No instructions → no dependencies
	// HARDWARE: All 1024 flip-flops reset to 0
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < WindowSize; i++ {
		if sched.DepMatrix[i] != 0 {
			t.Errorf("Initial dep matrix row %d should be 0, got 0x%08X", i, sched.DepMatrix[i])
		}
	}
}

func TestDepMatrix_EnterSingle(t *testing.T) {
	// WHAT: Enter single instruction, no dependencies
	// WHY: First instruction has no producers in window
	// HARDWARE: Update logic runs but finds no matches
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	op := Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.EnterInstruction(15, op)

	// No dependencies (no older instructions)
	for i := 0; i < WindowSize; i++ {
		if sched.DepMatrix[i] != 0 {
			t.Errorf("Single instruction should have no dependencies, row %d = 0x%08X", i, sched.DepMatrix[i])
		}
	}

	// UnissuedValid should have bit 15 set
	if (sched.UnissuedValid>>15)&1 != 1 {
		t.Errorf("UnissuedValid should have bit 15 set, got 0x%08X", sched.UnissuedValid)
	}
}

func TestDepMatrix_EnterWithDependency(t *testing.T) {
	// WHAT: Enter consumer that depends on existing producer
	// WHY: Core functionality - dependency detection on enter
	// HARDWARE: XOR comparators find match, matrix bit set
	// CATEGORY: [UNIT]
	//
	// EXAMPLE:
	//   Slot 20 (older): A writes R10
	//   Slot 10 (newer): B reads R10 - depends on A

	sched := NewOoOScheduler()

	// Enter producer first (at older/higher slot)
	producer := Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	sched.EnterInstruction(20, producer)

	// Enter consumer that reads producer's output (at newer/lower slot)
	consumer := Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}
	sched.EnterInstruction(10, consumer)

	// Check dependency: matrix[20] bit 10 should be set
	// "Instruction at slot 10 depends on instruction at slot 20"
	if (sched.DepMatrix[20]>>10)&1 != 1 {
		t.Errorf("Consumer (slot 10) should depend on producer (slot 20), matrix[20]=0x%08X", sched.DepMatrix[20])
	}

	// Verify no reverse dependency (younger cannot be producer)
	if (sched.DepMatrix[10]>>20)&1 != 0 {
		t.Errorf("Producer should not depend on consumer, matrix[10]=0x%08X", sched.DepMatrix[10])
	}
}

func TestDepMatrix_EnterRAW_Src1(t *testing.T) {
	// WHAT: Consumer reads producer's output via Src1
	// WHY: Test Src1 path independently
	// HARDWARE: XOR(producer.Dest, consumer.Src1) produces zero → match
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 20})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 20, Src2: 3, Dest: 21})

	if (sched.DepMatrix[25]>>15)&1 != 1 {
		t.Errorf("RAW via Src1 not detected, matrix[25]=0x%08X", sched.DepMatrix[25])
	}
}

func TestDepMatrix_EnterRAW_Src2(t *testing.T) {
	// WHAT: Consumer reads producer's output via Src2
	// WHY: Test Src2 path independently
	// HARDWARE: XOR(producer.Dest, consumer.Src2) produces zero → match
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 20})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 3, Src2: 20, Dest: 21})

	if (sched.DepMatrix[25]>>15)&1 != 1 {
		t.Errorf("RAW via Src2 not detected, matrix[25]=0x%08X", sched.DepMatrix[25])
	}
}

func TestDepMatrix_EnterRAW_BothSources(t *testing.T) {
	// WHAT: Consumer reads producer's output via both sources
	// WHY: Some instructions use same source twice (e.g., R5 = R3 * R3)
	// HARDWARE: Both XORs produce zero, OR combines them (still one dependency)
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 20})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 20, Src2: 20, Dest: 21})

	if (sched.DepMatrix[25]>>15)&1 != 1 {
		t.Errorf("RAW via both sources not detected, matrix[25]=0x%08X", sched.DepMatrix[25])
	}

	// Should be exactly one dependent (not double-counted)
	if bits.OnesCount32(sched.DepMatrix[25]) != 1 {
		t.Errorf("Should be exactly one dependent, got %d", bits.OnesCount32(sched.DepMatrix[25]))
	}
}

func TestDepMatrix_EnterChain(t *testing.T) {
	// WHAT: Linear dependency chain A→B→C
	// WHY: Common pattern - sequential computation
	// HARDWARE: Creates two separate dependencies
	// CATEGORY: [UNIT] [PATTERN]
	//
	// EXAMPLE:
	//   Slot 25 (oldest): A writes R10
	//   Slot 20 (middle): B reads R10, writes R11
	//   Slot 15 (newest): C reads R11

	sched := NewOoOScheduler()

	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12})

	// B depends on A
	if (sched.DepMatrix[25]>>20)&1 != 1 {
		t.Errorf("B should depend on A, matrix[25]=0x%08X", sched.DepMatrix[25])
	}

	// C depends on B
	if (sched.DepMatrix[20]>>15)&1 != 1 {
		t.Errorf("C should depend on B, matrix[20]=0x%08X", sched.DepMatrix[20])
	}

	// C should NOT directly depend on A (no transitive closure)
	if (sched.DepMatrix[25]>>15)&1 != 0 {
		t.Errorf("C should NOT directly depend on A, matrix[25]=0x%08X", sched.DepMatrix[25])
	}

	// C has no dependents (it's a leaf)
	if sched.DepMatrix[15] != 0 {
		t.Errorf("C should have no dependents, matrix[15]=0x%08X", sched.DepMatrix[15])
	}
}

func TestDepMatrix_EnterDiamond(t *testing.T) {
	// WHAT: Diamond dependency pattern A→{B,C}→D
	// WHY: Common in expressions like D = f(B(A), C(A))
	// HARDWARE: A has two dependents, B and C each have one
	// CATEGORY: [UNIT] [PATTERN]
	//
	// VISUAL:
	//       A (slot 30)
	//      / \
	//     B   C (slots 25, 20)
	//      \ /
	//       D (slot 15)

	sched := NewOoOScheduler()

	sched.EnterInstruction(30, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})   // A
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})  // B depends on A
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 4, Dest: 12})  // C depends on A
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 11, Src2: 12, Dest: 13}) // D depends on B and C

	// A has B and C as dependents
	if (sched.DepMatrix[30]>>25)&1 != 1 {
		t.Error("B should depend on A")
	}
	if (sched.DepMatrix[30]>>20)&1 != 1 {
		t.Error("C should depend on A")
	}

	// D depends on B and C
	if (sched.DepMatrix[25]>>15)&1 != 1 {
		t.Error("D should depend on B")
	}
	if (sched.DepMatrix[20]>>15)&1 != 1 {
		t.Error("D should depend on C")
	}

	// D is a leaf (no dependents)
	if sched.DepMatrix[15] != 0 {
		t.Errorf("D should have no dependents, matrix[15]=0x%08X", sched.DepMatrix[15])
	}
}

func TestDepMatrix_EnterMultipleConsumers(t *testing.T) {
	// WHAT: One producer, many consumers
	// WHY: Common pattern - computed value used multiple times
	// HARDWARE: Producer's row has multiple bits set
	// CATEGORY: [UNIT] [PATTERN]

	sched := NewOoOScheduler()

	// Producer at slot 30
	sched.EnterInstruction(30, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// 4 consumers at slots 25, 20, 15, 10
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 4, Dest: 12})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 5, Dest: 13})
	sched.EnterInstruction(10, Operation{Valid: true, Src1: 10, Src2: 6, Dest: 14})

	expected := uint32((1 << 25) | (1 << 20) | (1 << 15) | (1 << 10))
	if sched.DepMatrix[30] != expected {
		t.Errorf("Expected 4 dependents, got matrix[30]=0x%08X", sched.DepMatrix[30])
	}
}

func TestDepMatrix_RetireClearsRow(t *testing.T) {
	// WHAT: Retiring instruction clears its row in dependency matrix
	// WHY: Retired instruction is no longer a producer
	// HARDWARE: Clear entire row on retire: matrix[slot] = 0
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// Create chain A→B
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Verify dependency exists
	if (sched.DepMatrix[25]>>20)&1 != 1 {
		t.Error("Dependency should exist before retire")
	}

	// Retire producer
	sched.RetireInstruction(25)

	// Row should be cleared
	if sched.DepMatrix[25] != 0 {
		t.Errorf("Retired instruction's row should be cleared, got 0x%08X", sched.DepMatrix[25])
	}
}

func TestDepMatrix_RetireClearsColumn(t *testing.T) {
	// WHAT: Retiring instruction clears its column in dependency matrix
	// WHY: Retired instruction is no longer a consumer
	// HARDWARE: Clear bit in each row: matrix[j][slot] = 0 for all j
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// Create situation where slot 15 depends on slot 25
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Verify dependency
	if (sched.DepMatrix[25]>>15)&1 != 1 {
		t.Error("Dependency should exist before retire")
	}

	// Retire consumer
	sched.RetireInstruction(15)

	// Column should be cleared
	if (sched.DepMatrix[25]>>15)&1 != 0 {
		t.Errorf("Retired instruction's column should be cleared, matrix[25]=0x%08X", sched.DepMatrix[25])
	}
}

func TestDepMatrix_RetireClearsUnissuedValid(t *testing.T) {
	// WHAT: Retiring instruction clears UnissuedValid bit
	// WHY: Retired instruction no longer blocks dependents
	// HARDWARE: Clear single bit in bitmap
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	if (sched.UnissuedValid>>20)&1 != 1 {
		t.Error("UnissuedValid should have bit 20 set after enter")
	}

	sched.RetireInstruction(20)

	if (sched.UnissuedValid>>20)&1 != 0 {
		t.Errorf("UnissuedValid should clear bit 20 after retire, got 0x%08X", sched.UnissuedValid)
	}
}

func TestDepMatrix_SlotIndexBoundaries(t *testing.T) {
	// WHAT: Dependencies between slot 31 (oldest) and slot 0 (newest)
	// WHY: Maximum slot index difference, validates comparison logic
	// HARDWARE: 5-bit comparison at extremes
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(0, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	if (sched.DepMatrix[31]>>0)&1 != 1 {
		t.Errorf("Slot 0 should depend on slot 31, matrix[31]=0x%08X", sched.DepMatrix[31])
	}
}

func TestDepMatrix_AdjacentSlots(t *testing.T) {
	// WHAT: Dependency between adjacent slots
	// WHY: Minimum slot index difference (off-by-one check)
	// HARDWARE: Age check must handle i = j+1
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(16, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	if (sched.DepMatrix[16]>>15)&1 != 1 {
		t.Errorf("Adjacent slot dependency not detected, matrix[16]=0x%08X", sched.DepMatrix[16])
	}
}

func TestDepMatrix_NoSelfDependency(t *testing.T) {
	// WHAT: No instruction depends on itself
	// WHY: Self-dependency is impossible (can't read own output before producing it)
	// HARDWARE: Only check older slots (i > j), so diagonal is always 0
	// CATEGORY: [UNIT] [INVARIANT]

	sched := NewOoOScheduler()

	// Enter instruction that reads and writes same register
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 10, Dest: 10})

	// Diagonal should be 0 (no self-dependency)
	if (sched.DepMatrix[15]>>15)&1 != 0 {
		t.Errorf("Self-dependency should not exist, matrix[15]=0x%08X", sched.DepMatrix[15])
	}
}

func TestDepMatrix_OutOfOrderInsertion(t *testing.T) {
	// WHAT: Enter younger slot before older slot
	// WHY: Validates incremental update handles both directions
	// HARDWARE: Must check both producer→consumer and consumer→producer on enter
	// CATEGORY: [UNIT] [INTEGRATION]
	//
	// SCENARIO:
	//   1. Enter slot 10 (younger) - reads R5
	//   2. Enter slot 20 (older) - writes R5
	//   Dependency slot 20 → slot 10 must be detected

	sched := NewOoOScheduler()

	// Enter younger first (slot 10), reads R5
	sched.EnterInstruction(10, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 11})

	// Enter older second (slot 20), writes R5
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})

	// Slot 10 should depend on slot 20
	if (sched.DepMatrix[20]>>10)&1 != 1 {
		t.Error("Dependency missed with out-of-order insertion")
	}
}

func TestDepMatrix_OutOfOrderInsertion_Chain(t *testing.T) {
	// WHAT: Build chain in reverse order
	// WHY: Stress test incremental updates with reverse insertion
	// CATEGORY: [UNIT] [STRESS]

	sched := NewOoOScheduler()

	// Enter C first (youngest), then B, then A (oldest)
	sched.EnterInstruction(10, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12}) // C reads R11
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11}) // B reads R10, writes R11
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})  // A writes R10

	// A→B dependency
	if (sched.DepMatrix[20]>>15)&1 != 1 {
		t.Error("A→B dependency missed")
	}

	// B→C dependency
	if (sched.DepMatrix[15]>>10)&1 != 1 {
		t.Error("B→C dependency missed")
	}
}

func TestDepMatrix_EnterAlreadyIssuedProducer(t *testing.T) {
	// WHAT: Enter consumer when producer exists but is already issued
	// WHY: Should NOT create dependency (producer already in flight, not blocking)
	// HARDWARE: UpdateDependenciesOnEnter checks !producer.Issued
	// CATEGORY: [UNIT] [INTEGRATION]

	sched := NewOoOScheduler()

	// Enter and issue producer
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.Window.Ops[25].Issued = true
	sched.UnissuedValid &= ^(uint32(1) << 25)

	// Now enter consumer
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Dependency should NOT be created (producer already issued)
	if (sched.DepMatrix[25]>>15)&1 != 0 {
		t.Error("Should not create dependency to already-issued producer")
	}
}

func TestDepMatrix_AllSlotsDependent(t *testing.T) {
	// WHAT: Maximum fan-out - slot 31 has 31 dependents
	// WHY: Stress test row capacity
	// HARDWARE: Validates full 32-bit row can be populated
	// CATEGORY: [UNIT] [STRESS] [BOUNDARY]

	sched := NewOoOScheduler()

	// Producer at slot 31
	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// 31 consumers at slots 0-30
	for i := 0; i < 31; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 10, Src2: 3, Dest: uint8(20 + i)})
	}

	// All bits 0-30 should be set in row 31
	expected := uint32(0x7FFFFFFF) // bits 0-30
	if sched.DepMatrix[31] != expected {
		t.Errorf("Expected 31 dependents, got matrix[31]=0x%08X", sched.DepMatrix[31])
	}
}

func TestDepMatrix_CircularRegisterUse(t *testing.T) {
	// WHAT: A writes R5, B reads R5 writes R6, C reads R6 writes R5
	// WHY: Verify no confusion with register reuse across instructions
	// HARDWARE: Each dependency is tracked independently
	// CATEGORY: [UNIT] [PATTERN]
	//
	// This tests that the dependency matrix tracks instruction relationships,
	// not register lifetime. C depends on B (via R6), not on A (via R5),
	// even though both A and C write R5.

	sched := NewOoOScheduler()

	sched.EnterInstruction(30, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5}) // A: -> R5
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6}) // B: R5 -> R6
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 6, Src2: 4, Dest: 5}) // C: R6 -> R5

	// A -> B (via R5)
	if (sched.DepMatrix[30]>>25)&1 != 1 {
		t.Error("B should depend on A (reads R5)")
	}

	// B -> C (via R6)
	if (sched.DepMatrix[25]>>20)&1 != 1 {
		t.Error("C should depend on B (reads R6)")
	}

	// A should NOT -> C (C reads R6, not R5)
	if (sched.DepMatrix[30]>>20)&1 != 0 {
		t.Error("C should NOT depend on A (reads R6, not R5)")
	}
}

func TestDepMatrix_MultipleProducersSameRegister(t *testing.T) {
	// WHAT: Multiple older instructions write same register
	// WHY: Consumer should depend on ALL of them (WAW chain)
	// HARDWARE: All matching producers create dependencies
	// CATEGORY: [UNIT] [PATTERN]
	//
	// SCENARIO:
	//   Slot 30: writes R10
	//   Slot 25: writes R10
	//   Slot 20: writes R10
	//   Slot 15: reads R10
	//
	// Slot 15 depends on ALL of 30, 25, 20

	sched := NewOoOScheduler()

	sched.EnterInstruction(30, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 5, Src2: 6, Dest: 10})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 7, Dest: 11})

	// Slot 15 should depend on all three
	if (sched.DepMatrix[30]>>15)&1 != 1 {
		t.Error("Slot 15 should depend on slot 30")
	}
	if (sched.DepMatrix[25]>>15)&1 != 1 {
		t.Error("Slot 15 should depend on slot 25")
	}
	if (sched.DepMatrix[20]>>15)&1 != 1 {
		t.Error("Slot 15 should depend on slot 20")
	}
}

func TestDepMatrix_NoDependencyDifferentRegisters(t *testing.T) {
	// WHAT: Instructions using completely different registers
	// WHY: Verify no false dependencies
	// HARDWARE: XOR comparisons should all be non-zero
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 5, Src2: 6, Dest: 12})

	// No dependencies should exist
	for i := 0; i < WindowSize; i++ {
		if sched.DepMatrix[i] != 0 {
			t.Errorf("No dependencies expected, row %d = 0x%08X", i, sched.DepMatrix[i])
		}
	}
}

func TestDepMatrix_RetireMiddleOfChain(t *testing.T) {
	// WHAT: Retire middle instruction in A→B→C chain
	// WHY: Verify partial chain cleanup
	// HARDWARE: Row and column cleared for retired slot only
	// CATEGORY: [UNIT] [INTEGRATION]

	sched := NewOoOScheduler()

	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12})

	// Retire B (middle)
	sched.RetireInstruction(20)

	// A→B should be cleared (B retired)
	if (sched.DepMatrix[25]>>20)&1 != 0 {
		t.Error("A→B dependency should be cleared (B retired)")
	}

	// B→C should be cleared (B retired)
	if (sched.DepMatrix[20]>>15)&1 != 0 {
		t.Error("B→C dependency should be cleared (B retired)")
	}

	// A and C still exist (but no direct dependency between them)
	if !sched.Window.Ops[25].Valid || !sched.Window.Ops[15].Valid {
		// They might be valid or not depending on test setup
		// Main point is the dependencies are cleared
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 3. UNISSUED VALID BITMAP
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// UnissuedValid bitmap enables O(1) producer blocking check.
// UnissuedValid[i] = window[i].Valid AND NOT window[i].Issued
//
// Producer check becomes: (dep_matrix_column[slot] & UnissuedValid) != 0
// This replaces O(n) OR tree scan with AND + zero-detect.
//
// HARDWARE MAPPING:
//   - 32-bit register storing bitmap
//   - Set on instruction enter: OR with (1 << slot)
//   - Clear on instruction issue: AND with ~(1 << slot)
//   - Clear on instruction retire: AND with ~(1 << slot)
//   - Read: Direct wire fan-out for AND with dep_column
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestUnissuedValid_InitialEmpty(t *testing.T) {
	// WHAT: Fresh scheduler has empty UnissuedValid
	// WHY: No instructions → nothing unissued
	// HARDWARE: All bits reset to 0
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	if sched.UnissuedValid != 0 {
		t.Errorf("Initial UnissuedValid should be 0, got 0x%08X", sched.UnissuedValid)
	}
}

func TestUnissuedValid_SetOnEnter(t *testing.T) {
	// WHAT: EnterInstruction sets UnissuedValid bit
	// WHY: New instruction is valid and not yet issued
	// HARDWARE: OR sets single bit
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	if (sched.UnissuedValid>>20)&1 != 1 {
		t.Errorf("UnissuedValid[20] should be set, got 0x%08X", sched.UnissuedValid)
	}
}

func TestUnissuedValid_ClearedOnIssue(t *testing.T) {
	// WHAT: Issuing instruction clears UnissuedValid bit
	// WHY: Instruction is now issued, no longer blocks dependents
	// HARDWARE: UpdateAfterIssue clears the bit
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Issue it
	bundle := IssueBundle{
		Indices: [IssueWidth]uint8{20},
		Valid:   1,
	}
	sched.UpdateAfterIssue(bundle)

	if (sched.UnissuedValid>>20)&1 != 0 {
		t.Errorf("UnissuedValid[20] should be cleared after issue, got 0x%08X", sched.UnissuedValid)
	}
}

func TestUnissuedValid_ClearedOnRetire(t *testing.T) {
	// WHAT: Retiring instruction clears UnissuedValid bit
	// WHY: Instruction is gone from window
	// HARDWARE: RetireInstruction clears the bit
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.RetireInstruction(20)

	if (sched.UnissuedValid>>20)&1 != 0 {
		t.Errorf("UnissuedValid[20] should be cleared after retire, got 0x%08X", sched.UnissuedValid)
	}
}

func TestUnissuedValid_MultipleSlots(t *testing.T) {
	// WHAT: Multiple instructions set multiple bits
	// WHY: Real workload has many instructions
	// HARDWARE: Independent bit updates
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	slots := []int{5, 10, 15, 20, 25}
	for _, slot := range slots {
		sched.EnterInstruction(slot, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(slot)})
	}

	var expected uint32
	for _, slot := range slots {
		expected |= 1 << slot
	}

	if sched.UnissuedValid != expected {
		t.Errorf("UnissuedValid expected 0x%08X, got 0x%08X", expected, sched.UnissuedValid)
	}
}

func TestUnissuedValid_AllSlots(t *testing.T) {
	// WHAT: All 32 slots have unissued instructions
	// WHY: Verify full bitmap capacity
	// HARDWARE: All 32 bits set
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	for i := 0; i < 32; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(i)})
	}

	if sched.UnissuedValid != 0xFFFFFFFF {
		t.Errorf("All slots should be unissued, got 0x%08X", sched.UnissuedValid)
	}
}

func TestUnissuedValid_PartialIssue(t *testing.T) {
	// WHAT: Issue some instructions, verify others remain in bitmap
	// WHY: Selective issue should update only issued slots
	// HARDWARE: Each bit independent
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < 8; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	// Issue slots 0, 2, 4, 6 (even)
	bundle := IssueBundle{
		Indices: [IssueWidth]uint8{0, 2, 4, 6},
		Valid:   0b1111,
	}
	sched.UpdateAfterIssue(bundle)

	// Odd slots should still be set
	expected := uint32(0b10101010) // bits 1, 3, 5, 7
	if sched.UnissuedValid != expected {
		t.Errorf("Expected 0x%08X, got 0x%08X", expected, sched.UnissuedValid)
	}
}

func TestUnissuedValid_ReenterAfterRetire(t *testing.T) {
	// WHAT: Retire an instruction, enter new one in same slot
	// WHY: Slot reuse is normal operation
	// HARDWARE: Retire clears bit, enter sets it again
	// CATEGORY: [UNIT] [LIFECYCLE]

	sched := NewOoOScheduler()

	// Enter, retire, re-enter same slot
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.RetireInstruction(15)
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 20})

	if (sched.UnissuedValid>>15)&1 != 1 {
		t.Error("Re-entered slot should be in UnissuedValid")
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 4. BYPASS FORWARDING
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Bypass forwarding allows single-cycle ALU dependent chains.
// When instruction A issues, its destination is captured in LastIssuedDests.
// Next cycle, instruction B can read A's result via bypass (not scoreboard).
//
// Without bypass: 4 cycles (issue → execute → complete → scoreboard ready → issue)
// With bypass: 2 cycles (issue → bypass available → issue)
//
// HARDWARE MAPPING:
//   - LastIssuedDests: 16 × 6-bit registers
//   - LastIssuedValid: 16-bit register
//   - CheckBypass: 16 parallel 6-bit comparators + 16-way OR
//   - Updated by UpdateAfterIssue
//   - Cleared at start of each UpdateAfterIssue call
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestBypass_InitialEmpty(t *testing.T) {
	// WHAT: Fresh scheduler has no bypass available
	// WHY: No instructions have issued yet
	// HARDWARE: LastIssuedValid = 0
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	if sched.LastIssuedValid != 0 {
		t.Errorf("Initial LastIssuedValid should be 0, got 0x%04X", sched.LastIssuedValid)
	}

	// No bypass should be available for any register
	for i := uint8(0); i < 64; i++ {
		if sched.CheckBypass(i) {
			t.Errorf("No bypass should be available for register %d", i)
		}
	}
}

func TestBypass_CapturedOnIssue(t *testing.T) {
	// WHAT: Issuing instruction captures destination for bypass
	// WHY: Next cycle's consumers can use bypass
	// HARDWARE: UpdateAfterIssue populates LastIssuedDests
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Issue it
	bundle := IssueBundle{
		Indices: [IssueWidth]uint8{15},
		Valid:   1,
	}
	sched.UpdateAfterIssue(bundle)

	// Check bypass is available for R10
	if !sched.CheckBypass(10) {
		t.Error("Bypass should be available for R10 after issue")
	}

	// Check bypass is NOT available for other registers
	if sched.CheckBypass(11) {
		t.Error("Bypass should NOT be available for R11")
	}
}

func TestBypass_MultipleIssues(t *testing.T) {
	// WHAT: Multiple instructions issue, all captured for bypass
	// WHY: Up to 16 bypasses available per cycle
	// HARDWARE: 16 parallel capture registers
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < 5; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	bundle := IssueBundle{
		Indices: [IssueWidth]uint8{0, 1, 2, 3, 4},
		Valid:   0b11111,
	}
	sched.UpdateAfterIssue(bundle)

	for i := 0; i < 5; i++ {
		if !sched.CheckBypass(uint8(10 + i)) {
			t.Errorf("Bypass should be available for R%d", 10+i)
		}
	}
}

func TestBypass_ClearedOnNextIssue(t *testing.T) {
	// WHAT: Previous cycle's bypass cleared when new instructions issue
	// WHY: Bypass only valid for one cycle
	// HARDWARE: UpdateAfterIssue clears LastIssuedValid first
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// First issue
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	bundle1 := IssueBundle{Indices: [IssueWidth]uint8{20}, Valid: 1}
	sched.UpdateAfterIssue(bundle1)

	if !sched.CheckBypass(10) {
		t.Error("Bypass should be available for R10")
	}

	// Second issue (different dest)
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 20})
	bundle2 := IssueBundle{Indices: [IssueWidth]uint8{15}, Valid: 1}
	sched.UpdateAfterIssue(bundle2)

	// Old bypass should be gone
	if sched.CheckBypass(10) {
		t.Error("Old bypass should be cleared")
	}

	// New bypass should be available
	if !sched.CheckBypass(20) {
		t.Error("New bypass should be available")
	}
}

func TestBypass_EmptyBundleClearsAll(t *testing.T) {
	// WHAT: Empty issue bundle clears all bypasses
	// WHY: No new bypasses, old ones expire
	// HARDWARE: LastIssuedValid = 0 at start of UpdateAfterIssue
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// First issue
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	bundle1 := IssueBundle{Indices: [IssueWidth]uint8{20}, Valid: 1}
	sched.UpdateAfterIssue(bundle1)

	// Empty issue
	bundle2 := IssueBundle{Valid: 0}
	sched.UpdateAfterIssue(bundle2)

	if sched.CheckBypass(10) {
		t.Error("Bypass should be cleared after empty issue")
	}
}

func TestBypass_AllRegisters(t *testing.T) {
	// WHAT: Bypass works for all 64 registers
	// WHY: No register should be special-cased
	// HARDWARE: Full 6-bit register index
	// CATEGORY: [UNIT] [BOUNDARY]

	for reg := uint8(0); reg < 64; reg++ {
		sched := NewOoOScheduler()

		sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: reg})
		bundle := IssueBundle{Indices: [IssueWidth]uint8{15}, Valid: 1}
		sched.UpdateAfterIssue(bundle)

		if !sched.CheckBypass(reg) {
			t.Errorf("Bypass should work for register %d", reg)
		}
	}
}

func TestBypass_SameDestMultipleIssuers(t *testing.T) {
	// WHAT: Multiple ops issue same cycle, all writing different regs
	// WHY: Validates 16-wide bypass capture works correctly
	// HARDWARE: All 16 capture registers populated in parallel
	// CATEGORY: [UNIT] [STRESS]

	sched := NewOoOScheduler()

	for i := 0; i < 16; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	bundle := IssueBundle{Valid: 0xFFFF}
	for i := 0; i < 16; i++ {
		bundle.Indices[i] = uint8(i)
	}
	sched.UpdateAfterIssue(bundle)

	// All 16 bypasses should be available
	for i := 0; i < 16; i++ {
		if !sched.CheckBypass(uint8(10 + i)) {
			t.Errorf("Bypass should be available for R%d", 10+i)
		}
	}
}

func TestBypass_ConsumerReadsBothSourcesFromBypass(t *testing.T) {
	// WHAT: Consumer needs both Src1 and Src2 from bypass
	// WHY: Both bypass paths must work simultaneously
	// HARDWARE: Parallel comparator networks for Src1 and Src2
	// CATEGORY: [UNIT] [INTEGRATION]

	sched := NewOoOScheduler()

	// Two producers writing R10 and R11
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(24, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11})

	// Issue both producers
	bundle := IssueBundle{Indices: [IssueWidth]uint8{25, 24}, Valid: 0b11}
	sched.UpdateAfterIssue(bundle)

	// Consumer needs both R10 and R11
	sched.Window.Ops[15] = Operation{Valid: true, Src1: 10, Src2: 11, Dest: 12}
	sched.UnissuedValid |= 1 << 15

	// Consumer should be ready (both sources via bypass)
	bitmap := ComputeReadyBitmap(sched)

	if (bitmap>>15)&1 != 1 {
		t.Error("Consumer should be ready via dual bypass")
	}
}

func TestBypass_Register0(t *testing.T) {
	// WHAT: Bypass for register 0
	// WHY: Register 0 is LSB boundary, often special (zero register in RISC-V)
	// HARDWARE: Validates comparator handles register 0 correctly
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	sched.LastIssuedDests[0] = 0
	sched.LastIssuedValid = 1

	if !sched.CheckBypass(0) {
		t.Error("Bypass should work for register 0")
	}
}

func TestBypass_AllPositions(t *testing.T) {
	// WHAT: Bypass works from all 16 bundle positions
	// WHY: Validates full comparator network
	// HARDWARE: Each of 16 positions must be checked
	// CATEGORY: [UNIT] [BOUNDARY]

	for pos := 0; pos < 16; pos++ {
		sched := NewOoOScheduler()

		sched.LastIssuedDests[pos] = 42
		sched.LastIssuedValid = 1 << pos

		if !sched.CheckBypass(42) {
			t.Errorf("Bypass should work from position %d", pos)
		}

		// Verify other registers don't false-positive
		if sched.CheckBypass(41) {
			t.Errorf("Bypass should not match wrong register from position %d", pos)
		}
	}
}

func TestBypass_DuplicateDestInBundle(t *testing.T) {
	// WHAT: Two issued instructions have same destination (shouldn't happen in practice)
	// WHY: Verify bypass doesn't break with duplicate destinations
	// HARDWARE: Both comparators match, OR produces 1 (correct result)
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	// Manually set up duplicate destinations in bypass
	sched.LastIssuedDests[0] = 10
	sched.LastIssuedDests[1] = 10
	sched.LastIssuedValid = 0b11

	// Should still work
	if !sched.CheckBypass(10) {
		t.Error("Bypass should work even with duplicate destinations")
	}
}

func TestBypass_NoMatchWhenValidClear(t *testing.T) {
	// WHAT: Destination present but valid bit clear
	// WHY: Verify valid mask gates comparator output
	// HARDWARE: Comparator output ANDed with valid bit
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.LastIssuedDests[5] = 42
	sched.LastIssuedValid = 0 // No valid bits

	if sched.CheckBypass(42) {
		t.Error("Bypass should not match when valid is clear")
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 5. READY BITMAP
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// ComputeReadyBitmap identifies which instructions can issue right now.
// An instruction is ready when ALL of these are true:
//   1. It's valid (slot contains real instruction)
//   2. It's not already issued (prevents double-issue)
//   3. Src1 available: scoreboard[src1] OR bypass[src1]
//   4. Src2 available: scoreboard[src2] OR bypass[src2]
//   5. Destination ready: scoreboard[dest] = 1 (WAW check)
//   6. No unissued producers: (dep_matrix_column[slot] & UnissuedValid) == 0
//
// HARDWARE MAPPING:
//   - 32 parallel ready checkers (one per slot)
//   - Each checker: 6 AND inputs
//   - Scoreboard read: MUX tree
//   - Bypass check: 16 comparators + OR
//   - Producer check: AND + zero-detect
//   - Final: 32-bit ready bitmap
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestReadyBitmap_EmptyWindow(t *testing.T) {
	// WHAT: No valid instructions → no ready instructions
	// WHY: Base case - empty scheduler should produce no work
	// HARDWARE: All valid bits are 0, so all ready bits are 0
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 0 {
		t.Errorf("Empty window should have no ready ops, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_SingleReady(t *testing.T) {
	// WHAT: One instruction with sources and dest ready, no dependencies
	// WHY: Simplest positive case
	// HARDWARE: One ready checker outputs 1
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 5, Src2: 10, Dest: 15})

	// All registers already ready (NewOoOScheduler sets scoreboard to all 1s)
	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 1 {
		t.Errorf("Single ready op at slot 0 should give bitmap 0x1, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_MultipleReady(t *testing.T) {
	// WHAT: Multiple independent instructions, all ready
	// WHY: Verify parallel operation - all checkers work simultaneously
	// HARDWARE: Multiple ready checkers output 1
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < 3; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	bitmap := ComputeReadyBitmap(sched)

	expected := uint32(0b111)
	if bitmap != expected {
		t.Errorf("Expected bitmap 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_Src1NotReady(t *testing.T) {
	// WHAT: Instruction blocked on Src1
	// WHY: Verify AND logic - all conditions must be true
	// HARDWARE: scoreboard[src1] = 0 blocks ready
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 5, Src2: 10, Dest: 15})
	sched.Scoreboard.MarkPending(5) // Src1 not ready

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 0 {
		t.Errorf("Op with Src1 not ready should not be in bitmap, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_Src2NotReady(t *testing.T) {
	// WHAT: Instruction blocked on Src2
	// WHY: Symmetric with Src1 test
	// HARDWARE: scoreboard[src2] = 0 blocks ready
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 5, Src2: 10, Dest: 15})
	sched.Scoreboard.MarkPending(10) // Src2 not ready

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 0 {
		t.Errorf("Op with Src2 not ready should not be in bitmap, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_BothSourcesNotReady(t *testing.T) {
	// WHAT: Instruction blocked on both sources
	// WHY: Verify both paths checked
	// HARDWARE: Both scoreboard reads return 0
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 5, Src2: 10, Dest: 15})
	sched.Scoreboard.MarkPending(5)
	sched.Scoreboard.MarkPending(10)

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 0 {
		t.Errorf("Op with both sources not ready should not be in bitmap, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_DestNotReady_WAW(t *testing.T) {
	// WHAT: Instruction blocked because dest is being written (WAW)
	// WHY: This is the KEY test for WAW handling via scoreboard
	// HARDWARE: destReady = scoreboard[dest] = 0 blocks younger WAW
	// CATEGORY: [UNIT] [HAZARD]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 5, Src2: 10, Dest: 15})
	sched.Scoreboard.MarkPending(15) // Dest not ready (older writer in flight)

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 0 {
		t.Errorf("Op with dest not ready (WAW) should not be in bitmap, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_SkipsIssuedOps(t *testing.T) {
	// WHAT: Already-issued ops excluded from ready bitmap
	// WHY: Prevents double-issue - catastrophic bug if same instruction runs twice
	// HARDWARE: issued=1 gates the output to 0
	// CATEGORY: [UNIT] [INVARIANT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(1, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 11})

	// Mark op 0 as issued
	sched.Window.Ops[0].Issued = true
	sched.UnissuedValid &= ^uint32(1 << 0)

	bitmap := ComputeReadyBitmap(sched)

	expected := uint32(0b10) // Only op 1
	if bitmap != expected {
		t.Errorf("Should skip issued ops, expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_UnissuedProducerBlocks(t *testing.T) {
	// WHAT: Consumer blocked by unissued producer in window
	// WHY: This is the KEY test for incremental dependency matrix
	// HARDWARE: (dep_column & UnissuedValid) != 0 blocks ready
	// CATEGORY: [UNIT] [INTEGRATION]

	sched := NewOoOScheduler()

	// Producer at slot 20 (older, not issued)
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})

	// Consumer at slot 10 (younger, reads R5)
	sched.EnterInstruction(10, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6})

	bitmap := ComputeReadyBitmap(sched)

	// Only slot 20 should be ready (producer)
	// Slot 10 blocked by unissued producer
	expected := uint32(1 << 20)
	if bitmap != expected {
		t.Errorf("Consumer should be blocked by unissued producer, expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_IssuedProducerDoesNotBlock(t *testing.T) {
	// WHAT: Consumer ready via bypass after producer issues
	// WHY: This is CORRECT behavior - bypass enables single-cycle forwarding
	// HARDWARE: (scoreboard[src] OR bypass[src]) enables ready
	// CATEGORY: [UNIT] [INTEGRATION] [BYPASS]
	//
	// SCENARIO:
	//   Slot 20 (older): A writes R5 (ISSUED, bypass available)
	//   Slot 10 (newer): B reads R5
	//
	// A.Issued = true → B passes producer check (UnissuedValid[20] = 0)
	// scoreboard[5] = 0 BUT bypass[5] = 1 → B IS READY via bypass!

	sched := NewOoOScheduler()

	// Producer at slot 20
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})

	// Consumer at slot 10
	sched.EnterInstruction(10, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6})

	// Issue producer (sets up bypass, marks dest pending)
	bundle := IssueBundle{Indices: [IssueWidth]uint8{20}, Valid: 1}
	sched.UpdateAfterIssue(bundle)

	bitmap := ComputeReadyBitmap(sched)

	// Consumer SHOULD be ready via bypass!
	expected := uint32(1 << 10)
	if bitmap != expected {
		t.Errorf("Consumer should be ready via bypass, expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_BypassEnablesReady(t *testing.T) {
	// WHAT: Consumer becomes ready via bypass even when scoreboard shows pending
	// WHY: This is the KEY test for bypass forwarding integration
	// HARDWARE: (scoreboard[src] OR bypass[src]) enables ready
	// CATEGORY: [UNIT] [BYPASS]

	sched := NewOoOScheduler()

	// Producer at slot 20
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})

	// Issue producer (sets up bypass)
	bundle := IssueBundle{Indices: [IssueWidth]uint8{20}, Valid: 1}
	sched.UpdateAfterIssue(bundle)

	// Consumer at slot 10 (added after producer issued)
	// This consumer reads R5, which is:
	//   - Pending in scoreboard (producer issued but not complete)
	//   - Available via bypass (producer just issued)
	sched.Window.Ops[10] = Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6}
	sched.UnissuedValid |= 1 << 10

	bitmap := ComputeReadyBitmap(sched)

	// Consumer should be ready via bypass
	expected := uint32(1 << 10)
	if bitmap != expected {
		t.Errorf("Consumer should be ready via bypass, expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_FullWindow(t *testing.T) {
	// WHAT: All 32 slots filled and ready
	// WHY: Maximum parallelism case - stress test parallel checkers
	// HARDWARE: All 32 ready checkers active simultaneously
	// CATEGORY: [UNIT] [STRESS]

	sched := NewOoOScheduler()

	for i := 0; i < 32; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	bitmap := ComputeReadyBitmap(sched)

	expected := ^uint32(0) // All 32 bits set
	if bitmap != expected {
		t.Errorf("Full window should have all bits set, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_MixedReadiness(t *testing.T) {
	// WHAT: Mix of ready and blocked ops
	// WHY: Realistic scenario
	// HARDWARE: Each checker operates independently
	// CATEGORY: [UNIT] [PATTERN]

	sched := NewOoOScheduler()

	// Slot 0: All ready → READY
	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Slot 1: Src1 not ready → BLOCKED
	sched.EnterInstruction(1, Operation{Valid: true, Src1: 50, Src2: 2, Dest: 11})
	sched.Scoreboard.MarkPending(50)

	// Slot 2: Has unissued producer → BLOCKED
	sched.EnterInstruction(5, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 20})  // Producer
	sched.EnterInstruction(2, Operation{Valid: true, Src1: 20, Src2: 2, Dest: 12}) // Consumer

	// Slot 3: All ready → READY
	sched.EnterInstruction(3, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 13})

	bitmap := ComputeReadyBitmap(sched)

	// Ready: slots 0, 3, 5 (producer)
	expected := uint32((1 << 0) | (1 << 3) | (1 << 5))
	if bitmap != expected {
		t.Errorf("Mixed readiness: expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestReadyBitmap_SrcEqualsDest(t *testing.T) {
	// WHAT: Instruction where Src1 == Dest (increment pattern)
	// WHY: Common pattern: R5 = R5 + 1
	// HARDWARE: Same register checked for source availability AND dest availability
	// CATEGORY: [UNIT] [PATTERN]

	sched := NewOoOScheduler()

	// R5 = R5 + R2 (increment-like)
	sched.EnterInstruction(0, Operation{Valid: true, Src1: 5, Src2: 2, Dest: 5})

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 1 {
		t.Errorf("Self-referencing op should be ready, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_AllSourcesSamePendingReg(t *testing.T) {
	// WHAT: Both sources are same pending register
	// WHY: R5 = R3 * R3 where R3 is pending
	// HARDWARE: Both source checks return 0, AND produces 0
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.Scoreboard.MarkPending(3)
	sched.EnterInstruction(0, Operation{Valid: true, Src1: 3, Src2: 3, Dest: 5})

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 0 {
		t.Error("Op with both sources pending should not be ready")
	}
}

func TestReadyBitmap_ScoreboardReadyBypassAlsoReady(t *testing.T) {
	// WHAT: Register ready in BOTH scoreboard AND bypass
	// WHY: OR should still produce ready (not double-count or error)
	// HARDWARE: OR of two 1s is still 1
	// CATEGORY: [UNIT] [BYPASS]

	sched := NewOoOScheduler()

	// R10 ready in scoreboard (default)
	// Also set up bypass for R10
	sched.LastIssuedDests[0] = 10
	sched.LastIssuedValid = 1

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 10, Src2: 2, Dest: 15})

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 1 {
		t.Error("Op should be ready (both scoreboard and bypass say yes)")
	}
}

func TestReadyBitmap_OnlyBypassNoScoreboard(t *testing.T) {
	// WHAT: Register pending in scoreboard but available via bypass
	// WHY: Bypass should enable ready even when scoreboard says no
	// HARDWARE: OR(scoreboard=0, bypass=1) = 1
	// CATEGORY: [UNIT] [BYPASS]

	sched := NewOoOScheduler()

	// R10 pending in scoreboard
	sched.Scoreboard.MarkPending(10)

	// But available via bypass
	sched.LastIssuedDests[0] = 10
	sched.LastIssuedValid = 1

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 10, Src2: 2, Dest: 15})

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 1 {
		t.Error("Op should be ready via bypass despite scoreboard pending")
	}
}

func TestReadyBitmap_InvalidSlotNotReady(t *testing.T) {
	// WHAT: Invalid slot should not appear in ready bitmap
	// WHY: Empty slots have Valid=false
	// HARDWARE: Valid bit gates ready output
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// Don't enter anything - all slots invalid
	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 0 {
		t.Errorf("Invalid slots should not be ready, got 0x%08X", bitmap)
	}
}

func TestReadyBitmap_SparseWindow(t *testing.T) {
	// WHAT: Only some slots filled (sparse window)
	// WHY: Realistic - instructions enter and retire at different times
	// HARDWARE: Ready bitmap matches filled slots
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// Fill only slots 5, 15, 25
	sched.EnterInstruction(5, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11})
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 5, Src2: 6, Dest: 12})

	bitmap := ComputeReadyBitmap(sched)

	expected := uint32((1 << 5) | (1 << 15) | (1 << 25))
	if bitmap != expected {
		t.Errorf("Sparse window: expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 6. PRIORITY CLASSIFICATION
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// ClassifyPriority splits ready instructions into two tiers:
//   - High Priority: Instructions with dependents (blocking other work)
//   - Low Priority:  Instructions without dependents (leaves)
//
// This approximates critical path scheduling:
//   - Schedule blockers first to unblock dependent work ASAP
//   - Leaves can wait without delaying anything
//
// HARDWARE MAPPING:
//   - ComputeHasDependents: 32 parallel OR reductions (one per row)
//   - ClassifyPriority: 2 AND gates (32-bit each)
//   - High = ready AND hasDependents
//   - Low = ready AND NOT hasDependents
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestPriority_AllLeaves(t *testing.T) {
	// WHAT: All ready ops have no dependents
	// WHY: Fully independent workload - all low priority
	// HARDWARE: All hasDependents bits are 0
	// CATEGORY: [UNIT]

	readyBitmap := uint32(0b1111)
	hasDependents := uint32(0)

	priority := ClassifyPriority(readyBitmap, hasDependents)

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
	// HARDWARE: All hasDependents bits are 1
	// CATEGORY: [UNIT]

	readyBitmap := uint32(0b111)
	hasDependents := uint32(0b111)

	priority := ClassifyPriority(readyBitmap, hasDependents)

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
	// HARDWARE: Some hasDependents bits set, others clear
	// CATEGORY: [UNIT]

	readyBitmap := uint32(0b11111)
	hasDependents := uint32(0b00101) // Ops 0 and 2 have dependents

	priority := ClassifyPriority(readyBitmap, hasDependents)

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
	// CATEGORY: [UNIT]

	readyBitmap := uint32(0b001)
	hasDependents := uint32(0b111)

	priority := ClassifyPriority(readyBitmap, hasDependents)

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
	// CATEGORY: [UNIT]

	readyBitmap := uint32(0)
	hasDependents := uint32(0b111)

	priority := ClassifyPriority(readyBitmap, hasDependents)

	if priority.HighPriority != 0 || priority.LowPriority != 0 {
		t.Error("Empty ready bitmap should produce empty priority classes")
	}
}

func TestPriority_DisjointSets(t *testing.T) {
	// WHAT: High and low priority sets don't overlap
	// WHY: Each op is exactly one of: high, low, or not ready
	// HARDWARE: Mutual exclusion guaranteed by AND logic
	// CATEGORY: [UNIT] [INVARIANT]

	for _, readyBitmap := range []uint32{0, 0xFF, 0xFF00, 0xFFFFFFFF} {
		hasDependents := uint32(0x55555555)

		priority := ClassifyPriority(readyBitmap, hasDependents)

		// High and low should not overlap
		if priority.HighPriority&priority.LowPriority != 0 {
			t.Errorf("High and low overlap: H=0x%08X L=0x%08X", priority.HighPriority, priority.LowPriority)
		}

		// Union should equal ready bitmap
		union := priority.HighPriority | priority.LowPriority
		if union != readyBitmap {
			t.Errorf("Union should equal ready bitmap: U=0x%08X R=0x%08X", union, readyBitmap)
		}
	}
}

func TestPriority_FullWindow(t *testing.T) {
	// WHAT: All 32 slots ready with various dependents
	// WHY: Verify full bitmap handling
	// HARDWARE: All 32 priority checkers active
	// CATEGORY: [UNIT] [STRESS]

	readyBitmap := uint32(0xFFFFFFFF)
	hasDependents := uint32(0xAAAAAAAA) // Alternating

	priority := ClassifyPriority(readyBitmap, hasDependents)

	if priority.HighPriority != 0xAAAAAAAA {
		t.Errorf("High priority wrong, got 0x%08X", priority.HighPriority)
	}
	if priority.LowPriority != 0x55555555 {
		t.Errorf("Low priority wrong, got 0x%08X", priority.LowPriority)
	}
}

func TestComputeHasDependents_Empty(t *testing.T) {
	// WHAT: Empty dependency matrix
	// WHY: No dependencies → no instruction has dependents
	// HARDWARE: All OR reductions produce 0
	// CATEGORY: [UNIT]

	var depMatrix DependencyMatrix

	hasDependents := ComputeHasDependents(depMatrix)

	if hasDependents != 0 {
		t.Errorf("Empty dep matrix should have no dependents, got 0x%08X", hasDependents)
	}
}

func TestComputeHasDependents_SingleDependent(t *testing.T) {
	// WHAT: ComputeHasDependents correctly identifies rows with dependents
	// WHY: Verify the OR reduction per row
	// HARDWARE: 32 parallel OR reductions
	// CATEGORY: [UNIT]

	var depMatrix DependencyMatrix
	depMatrix[5] = 0b00100  // Slot 5 has slot 2 as dependent
	depMatrix[10] = 0b11000 // Slot 10 has slots 3 and 4 as dependents
	depMatrix[15] = 0       // Slot 15 has no dependents

	hasDependents := ComputeHasDependents(depMatrix)

	if (hasDependents>>5)&1 != 1 {
		t.Error("Slot 5 should have dependents")
	}
	if (hasDependents>>10)&1 != 1 {
		t.Error("Slot 10 should have dependents")
	}
	if (hasDependents>>15)&1 != 0 {
		t.Error("Slot 15 should not have dependents")
	}
}

func TestComputeHasDependents_AllSlots(t *testing.T) {
	// WHAT: Every slot has at least one dependent
	// WHY: Verify all 32 OR reductions work
	// HARDWARE: All 32 bits set in output
	// CATEGORY: [UNIT] [BOUNDARY]

	var depMatrix DependencyMatrix
	for i := 0; i < 32; i++ {
		depMatrix[i] = 1 // Each slot has slot 0 as dependent
	}

	hasDependents := ComputeHasDependents(depMatrix)

	if hasDependents != 0xFFFFFFFF {
		t.Errorf("All slots should have dependents, got 0x%08X", hasDependents)
	}
}

func TestComputeHasDependents_MaxDependents(t *testing.T) {
	// WHAT: Single slot has maximum dependents (all 32 bits set in row)
	// WHY: Verify OR reduction handles full row
	// HARDWARE: 32-input OR gate
	// CATEGORY: [UNIT] [BOUNDARY]

	var depMatrix DependencyMatrix
	depMatrix[15] = 0xFFFFFFFF // Slot 15 has all 32 as dependents

	hasDependents := ComputeHasDependents(depMatrix)

	if (hasDependents>>15)&1 != 1 {
		t.Error("Slot 15 should have dependents")
	}

	// Only slot 15 should be marked
	if hasDependents != (1 << 15) {
		t.Errorf("Only slot 15 should have dependents, got 0x%08X", hasDependents)
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 7. ISSUE SELECTION
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// SelectIssueBundle picks up to 16 instructions to execute:
//   1. If any high priority ops exist, select from high priority only
//   2. Otherwise, select from low priority
//   3. Within selected tier, pick oldest first (highest slot index)
//   4. Skip ops whose dest is already claimed this cycle (WAW within bundle)
//
// HARDWARE MAPPING:
//   - Tier selection: MUX based on (high != 0)
//   - Selection loop: 16 iterations of tree CLZ + conflict check
//   - Tree CLZ: O(log n) depth priority encoder
//   - Conflict check: 64-bit claimed mask, single bit lookup
//   - Output: 16 × 5-bit indices + 16-bit valid mask
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestIssueBundle_Empty(t *testing.T) {
	// WHAT: No ready ops → empty bundle
	// WHY: Nothing to issue
	// HARDWARE: Both tiers empty, valid mask = 0
	// CATEGORY: [UNIT]

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
	// CATEGORY: [UNIT]

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
	// CATEGORY: [UNIT]

	window := &InstructionWindow{}
	for i := 0; i < 5; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(10 + i)}
	}

	priority := PriorityClass{
		HighPriority: 0b00011, // Ops 0, 1
		LowPriority:  0b11100, // Ops 2, 3, 4
	}

	bundle := SelectIssueBundle(priority, window)

	// Should only select from high priority
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
	// CATEGORY: [UNIT]

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
	// CATEGORY: [UNIT]

	window := &InstructionWindow{}
	for i := 4; i < 8; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(10 + i)}
	}

	priority := PriorityClass{
		HighPriority: 0b11110000, // Ops 4,5,6,7
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	// First selected should be oldest (slot 7)
	if bundle.Indices[0] != 7 {
		t.Errorf("Oldest op (7) should be first, got %d", bundle.Indices[0])
	}

	// Verify descending order
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
	// CATEGORY: [UNIT] [BOUNDARY]

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
	// CATEGORY: [UNIT] [BOUNDARY]

	window := &InstructionWindow{}
	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(i)} // Different dests
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
	// CATEGORY: [UNIT] [HAZARD]

	window := &InstructionWindow{}
	window.Ops[15] = Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}
	window.Ops[10] = Operation{Valid: true, Src1: 3, Src2: 4, Dest: 10} // Same dest

	priority := PriorityClass{
		HighPriority: (1 << 15) | (1 << 10),
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	// Should select only 1 (oldest wins)
	count := bits.OnesCount16(bundle.Valid)
	if count != 1 {
		t.Errorf("Should select only 1 op (WAW conflict), got %d", count)
	}

	if bundle.Indices[0] != 15 {
		t.Errorf("Should select older slot 15, got slot %d", bundle.Indices[0])
	}
}

func TestIssueBundle_WAWConflict_Multiple(t *testing.T) {
	// WHAT: Multiple groups of WAW conflicts
	// WHY: Verify claimed tracking handles multiple conflicts
	// HARDWARE: 64-bit claimed mask tracks all 64 possible registers
	// CATEGORY: [UNIT] [HAZARD]

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

	// Should select 3 ops (one per dest: oldest from each group)
	count := bits.OnesCount16(bundle.Valid)
	if count != 3 {
		t.Errorf("Should select 3 ops (one per dest), got %d", count)
	}

	// Verify no duplicate destinations
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

func TestIssueBundle_NoDuplicates(t *testing.T) {
	// WHAT: Each selected index appears exactly once
	// WHY: Double-issue would be catastrophic
	// HARDWARE: Each CLZ iteration masks out selected bit
	// CATEGORY: [UNIT] [INVARIANT]

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

func TestIssueBundle_HighPriorityExhausted(t *testing.T) {
	// WHAT: High priority has fewer than 16 ops, verify we DON'T fill from low
	// WHY: Current design: one tier per cycle (important for critical path)
	// HARDWARE: Tier selection is a single MUX at start, not per-slot
	// CATEGORY: [UNIT]

	window := &InstructionWindow{}
	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(i)}
	}

	priority := PriorityClass{
		HighPriority: 0b1111,     // Only 4 high priority
		LowPriority:  0xFFFF0000, // 16 low priority
	}

	bundle := SelectIssueBundle(priority, window)

	// Should ONLY select the 4 high priority, NOT fill with low
	count := bits.OnesCount16(bundle.Valid)
	if count != 4 {
		t.Errorf("Should select only 4 high priority ops, got %d", count)
	}

	// Verify selected are from high priority (slots 0-3)
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		idx := bundle.Indices[i]
		if idx > 3 {
			t.Errorf("Should only select high priority slots 0-3, got slot %d", idx)
		}
	}
}

func TestIssueBundle_AllSameDestination(t *testing.T) {
	// WHAT: All 32 ops write same register
	// WHY: Extreme WAW conflict case
	// HARDWARE: Only oldest selected, all others blocked by claimed mask
	// CATEGORY: [UNIT] [BOUNDARY] [HAZARD]

	window := &InstructionWindow{}
	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: 10} // All write R10
	}

	priority := PriorityClass{
		HighPriority: 0xFFFFFFFF,
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	// Should select only 1 (oldest = slot 31)
	count := bits.OnesCount16(bundle.Valid)
	if count != 1 {
		t.Errorf("Should select only 1 op (all WAW conflict), got %d", count)
	}

	if bundle.Indices[0] != 31 {
		t.Errorf("Should select oldest slot 31, got slot %d", bundle.Indices[0])
	}
}

func TestIssueBundle_OnlyLowPriority(t *testing.T) {
	// WHAT: Only low priority ops available
	// WHY: Verify low priority tier works correctly
	// HARDWARE: Tier MUX selects low
	// CATEGORY: [UNIT]

	window := &InstructionWindow{}
	for i := 0; i < 8; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(10 + i)}
	}

	priority := PriorityClass{
		HighPriority: 0,
		LowPriority:  0xFF, // 8 ops
	}

	bundle := SelectIssueBundle(priority, window)

	count := bits.OnesCount16(bundle.Valid)
	if count != 8 {
		t.Errorf("Should select all 8 low priority ops, got %d", count)
	}
}

func TestIssueBundle_BoundarySlots(t *testing.T) {
	// WHAT: Selection from slot 0 and slot 31
	// WHY: Verify CLZ handles boundary slots correctly
	// HARDWARE: Priority encoder at extremes
	// CATEGORY: [UNIT] [BOUNDARY]

	window := &InstructionWindow{}
	window.Ops[0] = Operation{Valid: true, Dest: 10}
	window.Ops[31] = Operation{Valid: true, Dest: 11}

	priority := PriorityClass{
		HighPriority: (1 << 0) | (1 << 31),
		LowPriority:  0,
	}

	bundle := SelectIssueBundle(priority, window)

	count := bits.OnesCount16(bundle.Valid)
	if count != 2 {
		t.Errorf("Should select 2 ops, got %d", count)
	}

	// Slot 31 should be first (oldest)
	if bundle.Indices[0] != 31 {
		t.Errorf("Oldest (slot 31) should be first, got slot %d", bundle.Indices[0])
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 8. STATE UPDATES
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// UpdateAfterIssue: Called after SelectIssueBundle
//   - Marks dest registers pending (blocks RAW dependent ops)
//   - Sets Issued flag (prevents double-issue)
//   - Clears UnissuedValid bit (enables producer check to pass)
//   - Captures bypass info (enables next cycle's consumers)
//
// UpdateScoreboardAfterComplete: Called when execution units signal done
//   - Marks dest registers ready (unblocks dependent ops AND WAW ops)
//
// HARDWARE MAPPING:
//   - UpdateAfterIssue: 16 parallel update paths
//   - Each path: scoreboard write, window.Issued write, UnissuedValid clear, bypass capture
//   - UpdateScoreboardAfterComplete: 16 parallel MarkReady operations
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestUpdateAfterIssue_Single(t *testing.T) {
	// WHAT: Single op issued, verify all state changes
	// WHY: Basic state update verification
	// HARDWARE: All update paths exercised
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	bundle := IssueBundle{Indices: [IssueWidth]uint8{15}, Valid: 1}
	sched.UpdateAfterIssue(bundle)

	// Dest should be pending
	if sched.Scoreboard.IsReady(10) {
		t.Error("Dest register should be pending after issue")
	}

	// Issued flag should be set
	if !sched.Window.Ops[15].Issued {
		t.Error("Issued flag should be set")
	}

	// UnissuedValid should be cleared
	if (sched.UnissuedValid>>15)&1 != 0 {
		t.Error("UnissuedValid should be cleared")
	}

	// Bypass should be available
	if !sched.CheckBypass(10) {
		t.Error("Bypass should be available for dest")
	}
}

func TestUpdateAfterIssue_Multiple(t *testing.T) {
	// WHAT: Multiple ops issued in parallel
	// WHY: Typical case - up to 16 ops per cycle
	// HARDWARE: 16 parallel update paths
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < 5; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Dest: uint8(10 + i)})
	}

	bundle := IssueBundle{
		Indices: [IssueWidth]uint8{0, 1, 2, 3, 4},
		Valid:   0b11111,
	}
	sched.UpdateAfterIssue(bundle)

	for i := 0; i < 5; i++ {
		if sched.Scoreboard.IsReady(uint8(10 + i)) {
			t.Errorf("Register %d should be pending", 10+i)
		}
		if !sched.Window.Ops[i].Issued {
			t.Errorf("Op %d should be marked Issued", i)
		}
		if !sched.CheckBypass(uint8(10 + i)) {
			t.Errorf("Bypass should be available for register %d", 10+i)
		}
	}
}

func TestUpdateAfterIssue_EmptyBundle(t *testing.T) {
	// WHAT: Empty bundle doesn't change scoreboard or issued flags
	// WHY: No ops issued, minimal state change
	// HARDWARE: Valid mask gates all updates
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Dest: 10})

	bundle := IssueBundle{Valid: 0}
	sched.UpdateAfterIssue(bundle)

	if !sched.Scoreboard.IsReady(10) {
		t.Error("Empty bundle should not modify scoreboard")
	}
	if sched.Window.Ops[15].Issued {
		t.Error("Empty bundle should not set Issued flag")
	}
}

func TestUpdateAfterIssue_SparseBundle(t *testing.T) {
	// WHAT: Bundle with gaps (not all positions filled)
	// WHY: Common when fewer than 16 ops ready
	// HARDWARE: Valid mask gates each position independently
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < 8; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Dest: uint8(10 + i)})
	}

	// Issue only positions 0, 2, 4, 6 (even)
	bundle := IssueBundle{
		Indices: [IssueWidth]uint8{0, 0, 2, 0, 4, 0, 6, 0},
		Valid:   0b01010101, // Positions 0, 2, 4, 6
	}
	sched.UpdateAfterIssue(bundle)

	// Even slots should be issued
	for i := 0; i < 8; i += 2 {
		if !sched.Window.Ops[i].Issued {
			t.Errorf("Slot %d should be issued", i)
		}
	}

	// Odd slots should NOT be issued
	for i := 1; i < 8; i += 2 {
		if sched.Window.Ops[i].Issued {
			t.Errorf("Slot %d should NOT be issued", i)
		}
	}
}

func TestUpdateAfterIssue_ClearsOldBypass(t *testing.T) {
	// WHAT: New issue clears previous cycle's bypass
	// WHY: Bypass only valid for one cycle
	// HARDWARE: LastIssuedValid = 0 at start of UpdateAfterIssue
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// First issue
	sched.EnterInstruction(20, Operation{Valid: true, Dest: 10})
	bundle1 := IssueBundle{Indices: [IssueWidth]uint8{20}, Valid: 1}
	sched.UpdateAfterIssue(bundle1)

	if !sched.CheckBypass(10) {
		t.Error("First bypass should be available")
	}

	// Second issue (different op)
	sched.EnterInstruction(15, Operation{Valid: true, Dest: 20})
	bundle2 := IssueBundle{Indices: [IssueWidth]uint8{15}, Valid: 1}
	sched.UpdateAfterIssue(bundle2)

	// Old bypass should be cleared
	if sched.CheckBypass(10) {
		t.Error("Old bypass should be cleared")
	}

	// New bypass should be available
	if !sched.CheckBypass(20) {
		t.Error("New bypass should be available")
	}
}

func TestCompleteUpdate_Single(t *testing.T) {
	// WHAT: Single op completes, dest becomes ready
	// WHY: Basic completion handling
	// HARDWARE: One MarkReady
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()
	sched.Scoreboard.MarkPending(10)

	sched.UpdateScoreboardAfterComplete([IssueWidth]uint8{10}, 0b1)

	if !sched.Scoreboard.IsReady(10) {
		t.Error("Register 10 should be ready after completion")
	}
}

func TestCompleteUpdate_Multiple(t *testing.T) {
	// WHAT: Multiple ops complete in parallel
	// WHY: Typical case - variable latency ops complete together
	// HARDWARE: 16 parallel MarkReady operations
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < 5; i++ {
		sched.Scoreboard.MarkPending(uint8(10 + i))
	}

	destRegs := [IssueWidth]uint8{10, 11, 12, 13, 14}
	sched.UpdateScoreboardAfterComplete(destRegs, 0b11111)

	for i := 0; i < 5; i++ {
		if !sched.Scoreboard.IsReady(uint8(10 + i)) {
			t.Errorf("Register %d should be ready", 10+i)
		}
	}
}

func TestCompleteUpdate_Selective(t *testing.T) {
	// WHAT: Only some bundle positions complete
	// WHY: Variable latency - MUL takes 2 cycles, ADD takes 1
	// HARDWARE: completeMask gates which updates happen
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < 4; i++ {
		sched.Scoreboard.MarkPending(uint8(10 + i))
	}

	destRegs := [IssueWidth]uint8{10, 11, 12, 13}
	sched.UpdateScoreboardAfterComplete(destRegs, 0b1010) // Only indices 1 and 3

	if !sched.Scoreboard.IsReady(11) {
		t.Error("Register 11 (index 1) should be ready")
	}
	if !sched.Scoreboard.IsReady(13) {
		t.Error("Register 13 (index 3) should be ready")
	}
	if sched.Scoreboard.IsReady(10) {
		t.Error("Register 10 (index 0) should not be ready")
	}
	if sched.Scoreboard.IsReady(12) {
		t.Error("Register 12 (index 2) should not be ready")
	}
}

func TestCompleteUpdate_SparsePattern(t *testing.T) {
	// WHAT: Non-contiguous completion pattern
	// WHY: Real variable-latency completions are sparse
	// HARDWARE: Each bit in completeMask independent
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	for i := 0; i < 16; i++ {
		sched.Scoreboard.MarkPending(uint8(i))
	}

	destRegs := [IssueWidth]uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	// Complete only odd indices
	sched.UpdateScoreboardAfterComplete(destRegs, 0b1010101010101010)

	for i := 0; i < 16; i++ {
		expected := (i % 2) == 1
		if sched.Scoreboard.IsReady(uint8(i)) != expected {
			t.Errorf("Register %d: expected ready=%v", i, expected)
		}
	}
}

func TestCompleteUpdate_EmptyMask(t *testing.T) {
	// WHAT: No completions this cycle
	// WHY: All ops still in flight
	// HARDWARE: No updates happen
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.Scoreboard.MarkPending(10)
	sched.Scoreboard.MarkPending(11)

	destRegs := [IssueWidth]uint8{10, 11}
	sched.UpdateScoreboardAfterComplete(destRegs, 0) // Empty mask

	if sched.Scoreboard.IsReady(10) {
		t.Error("Register 10 should still be pending")
	}
	if sched.Scoreboard.IsReady(11) {
		t.Error("Register 11 should still be pending")
	}
}

func TestCompleteUpdate_AllComplete(t *testing.T) {
	// WHAT: All 16 bundle positions complete
	// WHY: All single-cycle ops (e.g., all ADDs)
	// HARDWARE: All 16 MarkReady operations
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	var destRegs [IssueWidth]uint8
	for i := 0; i < 16; i++ {
		sched.Scoreboard.MarkPending(uint8(i))
		destRegs[i] = uint8(i)
	}

	sched.UpdateScoreboardAfterComplete(destRegs, 0xFFFF)

	for i := 0; i < 16; i++ {
		if !sched.Scoreboard.IsReady(uint8(i)) {
			t.Errorf("Register %d should be ready", i)
		}
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 9. INSTRUCTION LIFECYCLE
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// EnterInstruction: Adds instruction to window, updates dependencies
// RetireInstruction: Removes instruction from window, clears dependencies
//
// Full lifecycle: Enter → Issue → Complete → Retire
//
// HARDWARE MAPPING:
//   - EnterInstruction: Window write + UpdateDependenciesOnEnter
//   - RetireInstruction: Window clear + UpdateDependenciesOnRetire
//   - State machine per slot: Invalid → Valid → Issued → Invalid
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestEnterInstruction_Basic(t *testing.T) {
	// WHAT: EnterInstruction populates window slot
	// WHY: Verify basic entry functionality
	// HARDWARE: Window write + dependency update
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	op := Operation{Valid: true, Src1: 5, Src2: 10, Dest: 15, Op: 42, Imm: 100}
	sched.EnterInstruction(20, op)

	if sched.Window.Ops[20] != op {
		t.Error("Window slot should contain entered operation")
	}
}

func TestEnterInstruction_SetsUnissuedValid(t *testing.T) {
	// WHAT: Enter sets UnissuedValid bit
	// WHY: New instruction is valid and not issued
	// HARDWARE: OR with (1 << slot)
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	if (sched.UnissuedValid>>20)&1 != 1 {
		t.Error("UnissuedValid bit should be set")
	}
}

func TestEnterInstruction_UpdatesDependencies(t *testing.T) {
	// WHAT: Enter updates dependency matrix
	// WHY: New instruction may depend on existing producers
	// HARDWARE: Triggers UpdateDependenciesOnEnter
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// Producer
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Consumer
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	if (sched.DepMatrix[25]>>20)&1 != 1 {
		t.Error("Dependency should be created on enter")
	}
}

func TestRetireInstruction_Basic(t *testing.T) {
	// WHAT: RetireInstruction clears window slot and dependencies
	// WHY: Verify basic retire functionality
	// HARDWARE: Window clear + dependency clear
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.RetireInstruction(20)

	if sched.Window.Ops[20].Valid {
		t.Error("Window slot should be invalid after retire")
	}
	if sched.Window.Ops[20].Issued {
		t.Error("Issued flag should be cleared after retire")
	}
}

func TestRetireInstruction_ClearsUnissuedValid(t *testing.T) {
	// WHAT: Retire clears UnissuedValid bit
	// WHY: Instruction no longer in window
	// HARDWARE: AND with ~(1 << slot)
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.RetireInstruction(20)

	if (sched.UnissuedValid>>20)&1 != 0 {
		t.Error("UnissuedValid bit should be cleared")
	}
}

func TestRetireInstruction_ClearsDependencies(t *testing.T) {
	// WHAT: Retire clears all dependencies involving this slot
	// WHY: Retired instruction no longer produces or consumes
	// HARDWARE: Clear row and column
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	sched.RetireInstruction(25)

	// Row should be cleared
	if sched.DepMatrix[25] != 0 {
		t.Error("Retired instruction's row should be cleared")
	}

	// Column should be cleared (bit 25 in all rows)
	for i := 0; i < 32; i++ {
		if (sched.DepMatrix[i]>>25)&1 != 0 {
			t.Errorf("Retired instruction's column bit should be cleared in row %d", i)
		}
	}
}

func TestInstructionLifecycle_Full(t *testing.T) {
	// WHAT: Full instruction lifecycle: enter → issue → complete → retire
	// WHY: End-to-end verification
	// HARDWARE: All state machines exercised
	// CATEGORY: [INTEGRATION] [LIFECYCLE]

	sched := NewOoOScheduler()

	// ENTER
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	if !sched.Window.Ops[15].Valid {
		t.Error("After enter: should be valid")
	}
	if (sched.UnissuedValid>>15)&1 != 1 {
		t.Error("After enter: should be in UnissuedValid")
	}

	// ISSUE
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if !sched.Window.Ops[15].Issued {
		t.Error("After issue: should be issued")
	}
	if sched.Scoreboard.IsReady(10) {
		t.Error("After issue: dest should be pending")
	}

	// COMPLETE
	var destRegs [IssueWidth]uint8
	destRegs[0] = 10
	sched.UpdateScoreboardAfterComplete(destRegs, bundle.Valid)

	if !sched.Scoreboard.IsReady(10) {
		t.Error("After complete: dest should be ready")
	}

	// RETIRE
	sched.RetireInstruction(15)

	if sched.Window.Ops[15].Valid {
		t.Error("After retire: should be invalid")
	}
}

func TestInstructionLifecycle_MultipleInFlight(t *testing.T) {
	// WHAT: Multiple instructions at different lifecycle stages
	// WHY: Realistic - instructions overlap in pipeline
	// HARDWARE: Multiple state machines active
	// CATEGORY: [INTEGRATION] [LIFECYCLE]

	sched := NewOoOScheduler()

	// Enter 4 instructions
	for i := 0; i < 4; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	// Issue 2
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	// Complete 1
	sched.UpdateScoreboardAfterComplete([IssueWidth]uint8{10}, 0b1)

	// Retire 1
	sched.RetireInstruction(int(bundle.Indices[0]))

	// Verify mixed states
	// One retired, some issued, some pending...
}

func TestState_RetireNonExistent(t *testing.T) {
	// WHAT: Retire a slot that was never filled
	// WHY: Should be safe no-op
	// HARDWARE: Clears already-empty state
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	// Retire empty slot - should not panic or corrupt state
	sched.RetireInstruction(15)

	if sched.Window.Ops[15].Valid {
		t.Error("Empty slot should remain invalid")
	}

	// UnissuedValid should still be 0
	if sched.UnissuedValid != 0 {
		t.Errorf("UnissuedValid should be 0, got 0x%08X", sched.UnissuedValid)
	}
}

func TestState_DoubleEnterSameSlot(t *testing.T) {
	// WHAT: Enter instruction to same slot twice (overwrite)
	// WHY: Real hardware might do this; verify state consistency
	// HARDWARE: Second write overwrites first
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 5, Src2: 6, Dest: 20})

	// Second instruction should be there
	if sched.Window.Ops[15].Dest != 20 {
		t.Error("Second enter should overwrite first")
	}

	// UnissuedValid should still have bit set
	if (sched.UnissuedValid>>15)&1 != 1 {
		t.Error("UnissuedValid should have bit 15 set")
	}
}

func TestState_DoubleRetireSameSlot(t *testing.T) {
	// WHAT: Retire same slot twice
	// WHY: Should be idempotent/safe
	// HARDWARE: Second clear is no-op
	// CATEGORY: [UNIT] [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.RetireInstruction(15)
	sched.RetireInstruction(15) // Double retire

	if sched.Window.Ops[15].Valid {
		t.Error("Slot should be invalid after double retire")
	}
}

func TestState_ReenterAfterRetire(t *testing.T) {
	// WHAT: Retire then re-enter same slot
	// WHY: Normal slot reuse
	// HARDWARE: Slot can be reused
	// CATEGORY: [UNIT] [LIFECYCLE]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.RetireInstruction(15)
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 5, Src2: 6, Dest: 20})

	if !sched.Window.Ops[15].Valid {
		t.Error("Re-entered slot should be valid")
	}
	if sched.Window.Ops[15].Dest != 20 {
		t.Error("Re-entered slot should have new destination")
	}
	if (sched.UnissuedValid>>15)&1 != 1 {
		t.Error("UnissuedValid should have bit set")
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 10. PIPELINE INTEGRATION
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// The scheduler is a 2-stage pipeline:
//   Cycle N:   ScheduleCycle0() computes priority → stored in PipelinedPriority
//   Cycle N+1: ScheduleCycle1() uses PipelinedPriority → returns bundle
//
// HARDWARE MAPPING:
//   - Pipeline register: PipelinedPriority (captured at cycle boundary)
//   - Cycle 0: Combinational ready/priority computation
//   - Cycle 1: Combinational issue selection + sequential state updates
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestNewOoOScheduler(t *testing.T) {
	// WHAT: Constructor initializes scoreboard to all-ready
	// WHY: Fresh context has no in-flight instructions
	// HARDWARE: All scoreboard bits set on context switch-in
	// CATEGORY: [UNIT]

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

func TestNewOoOScheduler_EmptyState(t *testing.T) {
	// WHAT: Constructor initializes all other state to zero/empty
	// WHY: Fresh context starts clean
	// HARDWARE: All flip-flops reset except scoreboard
	// CATEGORY: [UNIT]

	sched := NewOoOScheduler()

	// Window should be empty
	for i := 0; i < 32; i++ {
		if sched.Window.Ops[i].Valid {
			t.Errorf("Slot %d should be invalid", i)
		}
	}

	// Dependency matrix should be empty
	for i := 0; i < 32; i++ {
		if sched.DepMatrix[i] != 0 {
			t.Errorf("DepMatrix[%d] should be 0", i)
		}
	}

	// UnissuedValid should be empty
	if sched.UnissuedValid != 0 {
		t.Errorf("UnissuedValid should be 0")
	}

	// No bypass
	if sched.LastIssuedValid != 0 {
		t.Errorf("LastIssuedValid should be 0")
	}
}

func TestPipeline_BasicOperation(t *testing.T) {
	// WHAT: Cycle 0 computes priority, Cycle 1 uses it
	// WHY: Verify pipeline register transfers state
	// HARDWARE: D flip-flops capture priority at cycle boundary
	// CATEGORY: [UNIT] [PIPELINE]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

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
	// CATEGORY: [INTEGRATION] [PIPELINE]

	sched := NewOoOScheduler()

	for i := 0; i < 20; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	// First issue
	sched.ScheduleCycle0()
	bundle1 := sched.ScheduleCycle1()

	count1 := bits.OnesCount16(bundle1.Valid)
	if count1 != 16 {
		t.Errorf("First issue should select 16 ops, got %d", count1)
	}

	// Complete first batch
	var destRegs1 [IssueWidth]uint8
	for i := 0; i < 16; i++ {
		if (bundle1.Valid>>i)&1 != 0 {
			destRegs1[i] = sched.Window.Ops[bundle1.Indices[i]].Dest
		}
	}
	sched.UpdateScoreboardAfterComplete(destRegs1, bundle1.Valid)

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
	// CATEGORY: [INTEGRATION] [PIPELINE] [PATTERN]

	sched := NewOoOScheduler()

	// Chain: slot 25 → slot 20 → slot 15
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12})

	// Issue A only (B and C blocked by unissued producer)
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 25 {
		t.Errorf("Should issue A (slot 25) first, got slot %d", bundle.Indices[0])
	}
	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Error("Should issue only A (B and C blocked by producer check)")
	}

	// Complete A
	sched.ScheduleComplete([IssueWidth]uint8{10}, 0b1)

	// Issue B
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	foundB := false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 && bundle.Indices[i] == 20 {
			foundB = true
		}
	}
	if !foundB {
		t.Error("Should issue B after A completes")
	}

	// Complete B
	sched.ScheduleComplete([IssueWidth]uint8{11}, 0b1)

	// Issue C
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	foundC := false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 && bundle.Indices[i] == 15 {
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
	// CATEGORY: [INTEGRATION] [PIPELINE] [PATTERN]

	sched := NewOoOScheduler()

	sched.EnterInstruction(30, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 4, Dest: 12})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 11, Src2: 12, Dest: 13})

	// Issue A
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 30 {
		t.Errorf("Should issue A first, got slot %d", bundle.Indices[0])
	}

	// Complete A
	sched.ScheduleComplete([IssueWidth]uint8{10}, 0b1)

	// Issue B and C (parallel)
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	foundB, foundC := false, false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		switch bundle.Indices[i] {
		case 25:
			foundB = true
		case 20:
			foundC = true
		}
	}

	if !foundB || !foundC {
		t.Error("Should issue both B and C in parallel after A completes")
	}

	// Complete B and C
	sched.ScheduleComplete([IssueWidth]uint8{11, 12}, 0b11)

	// Issue D
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	foundD := false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 && bundle.Indices[i] == 15 {
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
	// CATEGORY: [UNIT] [PIPELINE]

	sched := NewOoOScheduler()

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Errorf("Empty window should produce empty bundle, got 0x%04X", bundle.Valid)
	}
}

func TestPipeline_StaleDataCheck(t *testing.T) {
	// WHAT: Verify pipeline doesn't use stale priority data
	// WHY: Pipeline register must be updated each cycle
	// HARDWARE: Priority captured at cycle 0 boundary
	// CATEGORY: [UNIT] [PIPELINE]

	sched := NewOoOScheduler()

	// First instruction
	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.ScheduleCycle0()
	bundle1 := sched.ScheduleCycle1()

	if bundle1.Valid == 0 {
		t.Error("First issue should select op")
	}

	// Second instruction (first is now issued)
	sched.EnterInstruction(1, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11})
	sched.ScheduleCycle0()
	bundle2 := sched.ScheduleCycle1()

	// Should not re-issue slot 0 (it's already issued)
	for i := 0; i < 16; i++ {
		if (bundle2.Valid>>i)&1 != 0 && bundle2.Indices[i] == 0 {
			t.Error("Should not re-issue already-issued instruction")
		}
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 11. DEPENDENCY PATTERNS
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Real programs exhibit various dependency patterns.
// Testing these patterns validates scheduler correctness.
//
// PATTERNS TESTED:
//   - Chain: A → B → C → D (serial)
//   - Diamond: A → {B, C} → D (fork-join)
//   - Forest: Multiple independent trees
//   - Wide tree: One root, many leaves
//   - Deep chain: Long serial dependency
//   - Reduction: Tree reduction (parallel → serial)
//   - Complex: Mix of above patterns
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestPattern_Forest(t *testing.T) {
	// WHAT: Multiple independent trees
	// WHY: Maximum ILP - trees can execute in parallel
	// CATEGORY: [PATTERN]

	sched := NewOoOScheduler()

	// Tree 1: A1 → B1
	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(28, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Tree 2: A2 → B2
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 4, Src2: 5, Dest: 20})
	sched.EnterInstruction(22, Operation{Valid: true, Src1: 20, Src2: 6, Dest: 21})

	// Tree 3: A3 → B3
	sched.EnterInstruction(19, Operation{Valid: true, Src1: 7, Src2: 8, Dest: 30})
	sched.EnterInstruction(16, Operation{Valid: true, Src1: 30, Src2: 9, Dest: 31})

	// Should issue all roots in parallel (A1, A2, A3)
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	count := bits.OnesCount16(bundle.Valid)
	if count != 3 {
		t.Errorf("Should issue 3 root nodes, got %d", count)
	}

	// Verify roots are selected (slots 31, 25, 19)
	selectedSlots := make(map[uint8]bool)
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 {
			selectedSlots[bundle.Indices[i]] = true
		}
	}
	if !selectedSlots[31] || !selectedSlots[25] || !selectedSlots[19] {
		t.Error("Should select all three root nodes")
	}
}

func TestPattern_WideTree(t *testing.T) {
	// WHAT: One root, many leaves
	// WHY: Single producer, multiple consumers
	// CATEGORY: [PATTERN]

	sched := NewOoOScheduler()

	// Root at slot 31
	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// 16 leaves at slots 15-0
	for i := 0; i < 16; i++ {
		sched.EnterInstruction(15-i, Operation{Valid: true, Src1: 10, Src2: 3, Dest: uint8(20 + i)})
	}

	// First issue: only root
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 || bundle.Indices[0] != 31 {
		t.Error("Should issue only root first")
	}

	// Complete root
	sched.ScheduleComplete([IssueWidth]uint8{10}, 0b1)

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
	// CATEGORY: [PATTERN] [STRESS]

	sched := NewOoOScheduler()

	// Chain of 20 ops
	for i := 0; i < 20; i++ {
		slot := 31 - i
		var src1 uint8
		if i == 0 {
			src1 = 1 // First op reads from ready register
		} else {
			src1 = uint8(9 + i) // Each subsequent reads previous output
		}
		sched.EnterInstruction(slot, Operation{Valid: true, Src1: src1, Src2: 2, Dest: uint8(10 + i)})
	}

	// Issue one at a time
	for step := 0; step < 20; step++ {
		expectedSlot := 31 - step

		sched.ScheduleCycle0()
		bundle := sched.ScheduleCycle1()

		if bits.OnesCount16(bundle.Valid) != 1 {
			t.Errorf("Step %d: should issue exactly 1 op, got %d", step, bits.OnesCount16(bundle.Valid))
		}

		if bundle.Indices[0] != uint8(expectedSlot) {
			t.Errorf("Step %d: expected slot %d, got %d", step, expectedSlot, bundle.Indices[0])
		}

		dest := sched.Window.Ops[expectedSlot].Dest
		sched.ScheduleComplete([IssueWidth]uint8{dest}, 0b1)
	}
}

func TestPattern_Reduction(t *testing.T) {
	// WHAT: Tree reduction pattern (parallel → serial)
	// WHY: Common in vector operations, sum reductions
	// CATEGORY: [PATTERN]
	//
	// STRUCTURE:
	//   Level 0: A, B, C, D (4 independent)
	//   Level 1: E=A+B, F=C+D (2 ops, each depends on 2 from level 0)
	//   Level 2: G=E+F (1 op, depends on both from level 1)

	sched := NewOoOScheduler()

	// Level 0: A, B, C, D (independent)
	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(30, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11})
	sched.EnterInstruction(29, Operation{Valid: true, Src1: 5, Src2: 6, Dest: 12})
	sched.EnterInstruction(28, Operation{Valid: true, Src1: 7, Src2: 8, Dest: 13})

	// Level 1: E=A+B, F=C+D
	sched.EnterInstruction(27, Operation{Valid: true, Src1: 10, Src2: 11, Dest: 14})
	sched.EnterInstruction(26, Operation{Valid: true, Src1: 12, Src2: 13, Dest: 15})

	// Level 2: G=E+F
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 14, Src2: 15, Dest: 16})

	// Level 0: all 4 in parallel
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 4 {
		t.Errorf("Level 0: should issue 4 ops, got %d", bits.OnesCount16(bundle.Valid))
	}

	// Complete level 0
	sched.ScheduleComplete([IssueWidth]uint8{10, 11, 12, 13}, 0b1111)

	// Level 1: both E and F
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 2 {
		t.Errorf("Level 1: should issue 2 ops, got %d", bits.OnesCount16(bundle.Valid))
	}

	// Complete level 1
	sched.ScheduleComplete([IssueWidth]uint8{14, 15}, 0b11)

	// Level 2: G
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 || bundle.Indices[0] != 25 {
		t.Error("Level 2: should issue G")
	}
}

func TestPattern_MixedDependencies(t *testing.T) {
	// WHAT: Complex mix of chains and independent ops
	// WHY: Realistic workload
	// CATEGORY: [PATTERN]
	//
	// STRUCTURE:
	//   Chain: A → B → C (slots 31, 30, 29)
	//   Independent: X, Y, Z (slots 25, 24, 23)
	//
	// Scheduler should prioritize A (has dependents) over X, Y, Z (leaves)

	sched := NewOoOScheduler()

	// Chain
	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(30, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(29, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12})

	// Independent
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 20, Src2: 21, Dest: 30})
	sched.EnterInstruction(24, Operation{Valid: true, Src1: 22, Src2: 23, Dest: 31})
	sched.EnterInstruction(23, Operation{Valid: true, Src1: 24, Src2: 25, Dest: 32})

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	// A (slot 31) should be issued (high priority - has dependents)
	// X, Y, Z are low priority (no dependents)
	// With current design, only high priority ops selected when available

	foundA := false
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 && bundle.Indices[i] == 31 {
			foundA = true
		}
	}
	if !foundA {
		t.Error("A should be issued (high priority)")
	}
}

func TestPattern_AllIndependent(t *testing.T) {
	// WHAT: All ops completely independent
	// WHY: Maximum ILP case
	// CATEGORY: [PATTERN]

	sched := NewOoOScheduler()

	// 16 independent ops
	for i := 0; i < 16; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	// All 16 should be issued
	if bits.OnesCount16(bundle.Valid) != 16 {
		t.Errorf("All independent ops should be issued, got %d", bits.OnesCount16(bundle.Valid))
	}
}

func TestPattern_AllDependent(t *testing.T) {
	// WHAT: Every op depends on previous
	// WHY: Minimum ILP case (fully serial)
	// CATEGORY: [PATTERN]

	sched := NewOoOScheduler()

	// Chain of 16 ops
	for i := 0; i < 16; i++ {
		var src1 uint8
		if i == 0 {
			src1 = 1
		} else {
			src1 = uint8(9 + i)
		}
		sched.EnterInstruction(31-i, Operation{Valid: true, Src1: src1, Src2: 2, Dest: uint8(10 + i)})
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	// Only first should be issued
	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Errorf("Only first of chain should be issued, got %d", bits.OnesCount16(bundle.Valid))
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 12. HAZARD HANDLING
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// CPU hazards are situations where naive execution would produce wrong results.
// SUPRAX handles all three hazard types:
//
//   RAW: Dependency matrix (producer must issue) + Scoreboard (producer must complete)
//        + Bypass (producer just issued, result available via forwarding)
//   WAR: Implicit in slot ordering (older reads before younger writes)
//   WAW: scoreboard[dest] check + claimed mask in SelectIssueBundle
//
// HARDWARE MAPPING:
//   - RAW detection: Comparators in UpdateDependenciesOnEnter
//   - RAW resolution: Ready bitmap blocks until scoreboard/bypass ready
//   - WAR: Not tracked (implicit in age ordering)
//   - WAW detection: scoreboard[dest] check in ComputeReadyBitmap
//   - WAW within bundle: claimed mask in SelectIssueBundle
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestHazard_RAW_Detected(t *testing.T) {
	// WHAT: Read-After-Write creates dependency
	// WHY: This is the fundamental dependency we track
	// CATEGORY: [UNIT] [HAZARD]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6})

	if (sched.DepMatrix[20]>>15)&1 != 1 {
		t.Error("RAW hazard not detected")
	}
}

func TestHazard_RAW_BlocksIssue(t *testing.T) {
	// WHAT: RAW hazard prevents consumer from issuing before producer
	// WHY: Consumer would read stale/invalid data
	// CATEGORY: [UNIT] [HAZARD]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6})

	bitmap := ComputeReadyBitmap(sched)

	// Producer ready, consumer blocked
	if (bitmap>>20)&1 != 1 {
		t.Error("Producer should be ready")
	}
	if (bitmap>>15)&1 != 0 {
		t.Error("Consumer should be blocked by RAW")
	}
}

func TestHazard_RAW_ResolvedByComplete(t *testing.T) {
	// WHAT: RAW hazard resolved when producer completes
	// WHY: Consumer can now read valid data from register
	// CATEGORY: [UNIT] [HAZARD]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6})

	// Issue producer
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Complete producer
	sched.ScheduleComplete([IssueWidth]uint8{5}, 0b1)

	// Consumer should now be ready (scoreboard says R5 ready)
	bitmap := ComputeReadyBitmap(sched)

	if (bitmap>>15)&1 != 1 {
		t.Error("Consumer should be ready after producer completes")
	}
}

func TestHazard_RAW_ResolvedByBypass(t *testing.T) {
	// WHAT: RAW hazard resolved by bypass forwarding
	// WHY: Consumer can read data from bypass network
	// CATEGORY: [UNIT] [HAZARD] [BYPASS]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6})

	// Issue producer (sets up bypass)
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Consumer should be ready via bypass (before completion)
	bitmap := ComputeReadyBitmap(sched)

	if (bitmap>>15)&1 != 1 {
		t.Error("Consumer should be ready via bypass")
	}
}

func TestHazard_WAR_NotTracked(t *testing.T) {
	// WHAT: Write-After-Read is NOT a true dependency
	// WHY: Reader already captured value before writer updates
	// CATEGORY: [UNIT] [HAZARD]
	//
	// SCENARIO:
	//   Slot 20 (older): reads R5
	//   Slot 15 (younger): writes R5
	// This is NOT a hazard because slot 20 (reader) is older and will
	// read R5 before slot 15 (writer) can modify it.

	sched := NewOoOScheduler()

	// Slot 20 reads R5, Slot 15 writes R5
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 5, Src2: 3, Dest: 6})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})

	// No dependency should exist (WAR not tracked)
	if sched.DepMatrix[20] != 0 {
		t.Errorf("WAR should not create dependency, matrix[20]=0x%08X", sched.DepMatrix[20])
	}
	if sched.DepMatrix[15] != 0 {
		t.Errorf("WAR reverse should not create dependency, matrix[15]=0x%08X", sched.DepMatrix[15])
	}

	// Both should be ready
	bitmap := ComputeReadyBitmap(sched)
	if (bitmap>>20)&1 != 1 {
		t.Error("Reader should be ready (WAR is not a hazard)")
	}
	if (bitmap>>15)&1 != 1 {
		t.Error("Writer should be ready (WAR is not a hazard)")
	}
}

func TestHazard_WAW_BlockedByScoreboard(t *testing.T) {
	// WHAT: Write-After-Write blocked by scoreboard[dest] check
	// WHY: Younger writer must wait for older writer to complete
	// CATEGORY: [INTEGRATION] [HAZARD]
	//
	// SCENARIO:
	//   Slot 20 (older): writes R5
	//   Slot 15 (younger): writes R5
	// Slot 15 cannot issue until Slot 20 completes (R5 becomes ready)

	sched := NewOoOScheduler()

	// Both write R5
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 5})

	// First cycle: claimed mask prevents same-cycle WAW
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Errorf("Should issue only 1 (claimed mask), got %d", bits.OnesCount16(bundle.Valid))
	}
	if bundle.Indices[0] != 20 {
		t.Errorf("Should issue older slot 20, got %d", bundle.Indices[0])
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
	sched.ScheduleComplete([IssueWidth]uint8{5}, 0b1)

	// Now younger can issue
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Indices[0] != 15 {
		t.Errorf("Younger should issue after older completes, got slot %d", bundle.Indices[0])
	}
}

func TestHazard_WAW_ClaimedMask(t *testing.T) {
	// WHAT: Two ready ops writing same register in same cycle
	// WHY: claimed mask prevents same-cycle WAW conflict
	// CATEGORY: [UNIT] [HAZARD]

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

func TestHazard_RAW_MultipleSources(t *testing.T) {
	// WHAT: Consumer has RAW hazard on both sources
	// WHY: Must wait for BOTH producers
	// CATEGORY: [UNIT] [HAZARD]

	sched := NewOoOScheduler()

	// Two producers
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(24, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11})

	// Consumer needs both
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 11, Dest: 12})

	bitmap := ComputeReadyBitmap(sched)

	// Both producers ready, consumer blocked
	if (bitmap>>25)&1 != 1 || (bitmap>>24)&1 != 1 {
		t.Error("Both producers should be ready")
	}
	if (bitmap>>20)&1 != 0 {
		t.Error("Consumer should be blocked (waiting on both)")
	}
}

func TestHazard_ChainedRAW(t *testing.T) {
	// WHAT: Chain of RAW hazards A → B → C → D
	// WHY: Each consumer must wait for its specific producer
	// CATEGORY: [UNIT] [HAZARD] [PATTERN]

	sched := NewOoOScheduler()

	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(30, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(29, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12})
	sched.EnterInstruction(28, Operation{Valid: true, Src1: 12, Src2: 5, Dest: 13})

	bitmap := ComputeReadyBitmap(sched)

	// Only A should be ready
	if bitmap != (1 << 31) {
		t.Errorf("Only first of chain should be ready, got 0x%08X", bitmap)
	}
}

func TestHazard_WAW_ChainedWriters(t *testing.T) {
	// WHAT: Multiple instructions writing same register
	// WHY: Must execute in order to get final correct value
	// CATEGORY: [UNIT] [HAZARD]

	sched := NewOoOScheduler()

	// Three writers to R10
	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(30, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 10})
	sched.EnterInstruction(29, Operation{Valid: true, Src1: 5, Src2: 6, Dest: 10})

	// First issue: only slot 31 (claimed mask)
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 31 {
		t.Error("First WAW should be oldest")
	}

	// Slot 30 and 29 blocked by scoreboard
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid != 0 {
		t.Error("Later WAW ops should be blocked")
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 13. EDGE CASES
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Edge cases test boundary conditions and unusual scenarios.
// These often reveal off-by-one errors, sign extension issues, or
// assumptions that don't hold at boundaries.
//
// CATEGORIES:
//   - Register boundaries (0, 63)
//   - Slot boundaries (0, 31)
//   - Empty states
//   - Full states
//   - Unusual combinations
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestEdge_Register0(t *testing.T) {
	// WHAT: Operations using register 0
	// WHY: Register 0 is LSB boundary
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 0, Src2: 0, Dest: 0})

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 1 {
		t.Error("Op using register 0 should be issuable")
	}
}

func TestEdge_Register63(t *testing.T) {
	// WHAT: Operations using register 63
	// WHY: Register 63 is MSB boundary
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 63, Src2: 63, Dest: 63})

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid != 1 {
		t.Error("Op using register 63 should be issuable")
	}
}

func TestEdge_Slot0(t *testing.T) {
	// WHAT: Op at slot 0 (newest position)
	// WHY: Lowest slot index boundary
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 1 {
		t.Errorf("Slot 0 should be in ready bitmap, got 0x%08X", bitmap)
	}
}

func TestEdge_Slot31(t *testing.T) {
	// WHAT: Op at slot 31 (oldest position)
	// WHY: Highest slot index boundary
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != (1 << 31) {
		t.Errorf("Slot 31 should be in ready bitmap, got 0x%08X", bitmap)
	}
}

func TestEdge_AllSlotsReady(t *testing.T) {
	// WHAT: All 32 slots ready with no dependencies
	// WHY: Maximum parallelism stress test
	// CATEGORY: [BOUNDARY] [STRESS]

	sched := NewOoOScheduler()

	for i := 0; i < 32; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	count := bits.OnesCount16(bundle.Valid)
	if count != 16 {
		t.Errorf("Should issue 16 (max), got %d", count)
	}
}

func TestEdge_SameRegisterAllOps(t *testing.T) {
	// WHAT: All ops read and write same register
	// WHY: Maximum register pressure scenario
	// CATEGORY: [BOUNDARY] [STRESS]

	sched := NewOoOScheduler()

	for i := 0; i < 10; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 5, Src2: 5, Dest: 5})
	}

	// Should issue one (WAW claimed), then serialize
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 1 {
		t.Errorf("Should issue exactly 1 (WAW claimed), got %d", bits.OnesCount16(bundle.Valid))
	}
}

func TestEdge_AlternatingSlots(t *testing.T) {
	// WHAT: Only even slots filled
	// WHY: Sparse pattern at slot level
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	for i := 0; i < 32; i += 2 {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	bitmap := ComputeReadyBitmap(sched)

	// Should have bits 0, 2, 4, ... 30 set
	expected := uint32(0x55555555)
	if bitmap != expected {
		t.Errorf("Alternating slots: expected 0x%08X, got 0x%08X", expected, bitmap)
	}
}

func TestEdge_ConsecutiveRegisters(t *testing.T) {
	// WHAT: Ops using consecutive registers (R0-R31)
	// WHY: Tests register numbering
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	for i := 0; i < 16; i++ {
		sched.EnterInstruction(i, Operation{
			Valid: true,
			Src1:  uint8(i),
			Src2:  uint8(i + 1),
			Dest:  uint8(i + 32),
		})
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 16 {
		t.Errorf("All should be ready, got %d", bits.OnesCount16(bundle.Valid))
	}
}

func TestEdge_ZeroImmediate(t *testing.T) {
	// WHAT: Operation with zero immediate
	// WHY: Zero is a common edge case
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 3, Op: 0, Imm: 0})

	if sched.Window.Ops[0].Imm != 0 {
		t.Error("Immediate should be 0")
	}
}

func TestEdge_MaxImmediate(t *testing.T) {
	// WHAT: Operation with maximum immediate
	// WHY: Upper boundary of immediate field
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 3, Op: 255, Imm: 65535})

	if sched.Window.Ops[0].Imm != 65535 {
		t.Error("Immediate should be 65535")
	}
	if sched.Window.Ops[0].Op != 255 {
		t.Error("Opcode should be 255")
	}
}

func TestEdge_SrcEqualsDestDependency(t *testing.T) {
	// WHAT: Producer writes R5, consumer reads and writes R5
	// WHY: Self-modifying register pattern
	// CATEGORY: [BOUNDARY]

	sched := NewOoOScheduler()

	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 5, Src2: 5, Dest: 5})

	// Consumer depends on producer
	if (sched.DepMatrix[20]>>15)&1 != 1 {
		t.Error("Consumer should depend on producer")
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 14. CORRECTNESS INVARIANTS
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Invariants are properties that must ALWAYS hold.
// Violating an invariant means the scheduler is fundamentally broken.
//
// INVARIANTS TESTED:
//   - No double-issue (instruction issues exactly once)
//   - Dependencies respected (consumer waits for producer)
//   - WAW ordering (writes complete in program order)
//   - Priority disjoint (high ∩ low = ∅)
//   - Issued flag consistency
//   - Scoreboard consistency
//   - UnissuedValid consistency
//   - Dependency matrix age ordering
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestInvariant_NoDoubleIssue(t *testing.T) {
	// INVARIANT: An instruction issues exactly once
	// WHY: Double-issue corrupts architectural state
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// First issue
	sched.ScheduleCycle0()
	bundle1 := sched.ScheduleCycle1()

	if bundle1.Valid != 1 {
		t.Fatal("First issue should succeed")
	}

	// Second attempt - should not select already-issued instruction
	sched.ScheduleCycle0()
	bundle2 := sched.ScheduleCycle1()

	if bundle2.Valid != 0 {
		t.Fatal("INVARIANT VIOLATION: Issued instruction selected again!")
	}
}

func TestInvariant_DependenciesRespected(t *testing.T) {
	// INVARIANT: Consumer never issues before producer issues
	// WHY: Would read stale/invalid data
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12})

	// First issue - only slot 25 should be possible
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		idx := bundle.Indices[i]
		if idx == 20 || idx == 15 {
			t.Fatalf("INVARIANT VIOLATION: Consumer %d issued before producer!", idx)
		}
	}
}

func TestInvariant_WAWOrdering(t *testing.T) {
	// INVARIANT: WAW writes complete in program order (via scoreboard blocking)
	// WHY: Final register value must be from last writer
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	// Two writers to R5
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 5})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 5})

	// Issue older first
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 25 {
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
	// CATEGORY: [INVARIANT]

	for trial := 0; trial < 100; trial++ {
		readyBitmap := uint32(trial * 31337)
		hasDependents := uint32((trial + 1) * 7919)

		priority := ClassifyPriority(readyBitmap, hasDependents)

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
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	for i := 0; i < 10; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
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
}

func TestInvariant_ScoreboardConsistency(t *testing.T) {
	// INVARIANT: Scoreboard reflects in-flight status
	// WHY: Scoreboard drives readiness decisions
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	// Initially all ready
	for i := uint8(0); i < 64; i++ {
		if !sched.Scoreboard.IsReady(i) {
			t.Fatalf("INVARIANT VIOLATION: Fresh scoreboard has pending register!")
		}
	}

	sched.EnterInstruction(0, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Dest should be pending
	if sched.Scoreboard.IsReady(10) {
		t.Fatalf("INVARIANT VIOLATION: Issued dest still marked ready!")
	}

	// Complete
	sched.ScheduleComplete([IssueWidth]uint8{10}, 0b1)

	// Dest should be ready
	if !sched.Scoreboard.IsReady(10) {
		t.Fatalf("INVARIANT VIOLATION: Completed dest not marked ready!")
	}
}

func TestInvariant_UnissuedValidConsistency(t *testing.T) {
	// INVARIANT: UnissuedValid[i] = Valid[i] AND NOT Issued[i]
	// WHY: Drives producer blocking check
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	// Enter instruction
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	if (sched.UnissuedValid>>15)&1 != 1 {
		t.Fatal("INVARIANT VIOLATION: Entered instruction not in UnissuedValid!")
	}

	// Issue instruction
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	if (sched.UnissuedValid>>15)&1 != 0 {
		t.Fatal("INVARIANT VIOLATION: Issued instruction still in UnissuedValid!")
	}
}

func TestInvariant_DepMatrixNoReverseAge(t *testing.T) {
	// INVARIANT: dep_matrix[i][j] can only be set if i > j (producer older)
	// WHY: Younger instruction cannot be producer for older consumer
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	// Fill window with various dependencies
	for i := 0; i < 32; i++ {
		var src uint8
		if i > 0 {
			src = uint8(9 + (i % 10))
		} else {
			src = 1
		}
		sched.EnterInstruction(i, Operation{Valid: true, Src1: src, Src2: 2, Dest: uint8(10 + i)})
	}

	// Check invariant: matrix[i][j] == 1 implies i > j
	for i := 0; i < 32; i++ {
		for j := 0; j < 32; j++ {
			if (sched.DepMatrix[i]>>j)&1 == 1 {
				if i <= j {
					t.Fatalf("INVARIANT VIOLATION: dep[%d][%d]=1 but %d <= %d (producer must be older)", i, j, i, j)
				}
			}
		}
	}
}

func TestInvariant_NoDiagonalDependency(t *testing.T) {
	// INVARIANT: No instruction depends on itself (diagonal is always 0)
	// WHY: Self-dependency is impossible
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	// Fill with self-referencing operations
	for i := 0; i < 32; i++ {
		reg := uint8(i % 64)
		sched.EnterInstruction(i, Operation{Valid: true, Src1: reg, Src2: reg, Dest: reg})
	}

	// Check diagonal
	for i := 0; i < 32; i++ {
		if (sched.DepMatrix[i]>>i)&1 != 0 {
			t.Fatalf("INVARIANT VIOLATION: Self-dependency at slot %d!", i)
		}
	}
}

func TestInvariant_RetireCleanup(t *testing.T) {
	// INVARIANT: After retire, slot has no trace in any structure
	// WHY: Retired instruction is completely gone
	// CATEGORY: [INVARIANT]

	sched := NewOoOScheduler()

	// Create dependencies
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Retire slot 25
	sched.RetireInstruction(25)

	// Check complete cleanup
	if sched.Window.Ops[25].Valid {
		t.Fatal("INVARIANT VIOLATION: Retired slot still valid!")
	}
	if sched.DepMatrix[25] != 0 {
		t.Fatal("INVARIANT VIOLATION: Retired slot has dependents!")
	}
	for j := 0; j < 32; j++ {
		if (sched.DepMatrix[j]>>25)&1 != 0 {
			t.Fatalf("INVARIANT VIOLATION: Something depends on retired slot!")
		}
	}
	if (sched.UnissuedValid>>25)&1 != 0 {
		t.Fatal("INVARIANT VIOLATION: Retired slot in UnissuedValid!")
	}
}

func TestInvariant_BundleIndicesInRange(t *testing.T) {
	// INVARIANT: All bundle indices are valid slot numbers (0-31)
	// WHY: Out-of-range index would cause undefined behavior
	// CATEGORY: [INVARIANT]

	window := &InstructionWindow{}
	for i := 0; i < 32; i++ {
		window.Ops[i] = Operation{Valid: true, Dest: uint8(i)}
	}

	priority := PriorityClass{HighPriority: 0xFFFFFFFF, LowPriority: 0}
	bundle := SelectIssueBundle(priority, window)

	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 == 0 {
			continue
		}
		if bundle.Indices[i] >= 32 {
			t.Fatalf("INVARIANT VIOLATION: Bundle index %d out of range: %d", i, bundle.Indices[i])
		}
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 15. STRESS TESTS
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Stress tests push the scheduler to limits with high-volume operations.
// These tests verify stability and correctness under load.
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestStress_RepeatedIssueCycles(t *testing.T) {
	// WHAT: Many issue cycles with refilling window
	// WHY: Steady-state behavior
	// CATEGORY: [STRESS]

	sched := NewOoOScheduler()
	totalIssued := 0
	opCounter := 0

	for cycle := 0; cycle < 100; cycle++ {
		// Fill empty slots
		for i := 0; i < 32; i++ {
			if !sched.Window.Ops[i].Valid || sched.Window.Ops[i].Issued {
				// Retire old instruction if issued
				if sched.Window.Ops[i].Valid && sched.Window.Ops[i].Issued {
					sched.RetireInstruction(i)
				}

				// Enter new instruction
				sched.EnterInstruction(i, Operation{
					Valid: true,
					Src1:  1,
					Src2:  2,
					Dest:  uint8((opCounter) % 64),
				})
				opCounter++
			}
		}

		sched.ScheduleCycle0()
		bundle := sched.ScheduleCycle1()

		count := bits.OnesCount16(bundle.Valid)
		totalIssued += count

		// Complete all issued ops
		var destRegs [IssueWidth]uint8
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
	// CATEGORY: [STRESS]

	sched := NewOoOScheduler()

	// Chain of 32 ops
	for i := 0; i < 32; i++ {
		slot := 31 - i
		var src1 uint8
		if i == 0 {
			src1 = 1
		} else {
			src1 = uint8(9 + i)
		}
		sched.EnterInstruction(slot, Operation{Valid: true, Src1: src1, Src2: 2, Dest: uint8(10 + i)})
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
		sched.ScheduleComplete([IssueWidth]uint8{dest}, 0b1)
	}
}

func TestStress_WAWHeavyWorkload(t *testing.T) {
	// WHAT: Many ops writing same registers
	// WHY: Stress test WAW handling
	// CATEGORY: [STRESS]

	sched := NewOoOScheduler()

	// 16 ops all write R5
	for i := 0; i < 16; i++ {
		sched.EnterInstruction(31-i, Operation{
			Valid: true,
			Src1:  uint8(10 + i),
			Src2:  uint8(30 + i),
			Dest:  5,
		})
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
		sched.ScheduleComplete([IssueWidth]uint8{5}, 0b1)
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

func TestStress_MixedWorkload(t *testing.T) {
	// WHAT: Mix of independent ops and dependency chains
	// WHY: Realistic workload pattern
	// CATEGORY: [STRESS]

	sched := NewOoOScheduler()

	// Independent ops (slots 31-26)
	for i := 0; i < 6; i++ {
		sched.EnterInstruction(31-i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	// Chain (slots 25-20)
	for i := 0; i < 6; i++ {
		var src1 uint8
		if i == 0 {
			src1 = 3
		} else {
			src1 = uint8(19 + i)
		}
		sched.EnterInstruction(25-i, Operation{Valid: true, Src1: src1, Src2: 4, Dest: uint8(20 + i)})
	}

	totalIssued := 0

	// Run until all issued
	for iter := 0; iter < 20 && totalIssued < 12; iter++ {
		sched.ScheduleCycle0()
		bundle := sched.ScheduleCycle1()

		count := bits.OnesCount16(bundle.Valid)
		totalIssued += count

		// Complete all
		var destRegs [IssueWidth]uint8
		for i := 0; i < 16; i++ {
			if (bundle.Valid>>i)&1 != 0 {
				destRegs[i] = sched.Window.Ops[bundle.Indices[i]].Dest
			}
		}
		sched.ScheduleComplete(destRegs, bundle.Valid)
	}

	if totalIssued != 12 {
		t.Errorf("Total issued should be 12, got %d", totalIssued)
	}
}

func TestStress_RapidEnterRetire(t *testing.T) {
	// WHAT: Rapid enter/retire cycling
	// WHY: Test slot reuse under pressure
	// CATEGORY: [STRESS]

	sched := NewOoOScheduler()

	for cycle := 0; cycle < 1000; cycle++ {
		slot := cycle % 32

		if sched.Window.Ops[slot].Valid {
			sched.RetireInstruction(slot)
		}

		sched.EnterInstruction(slot, Operation{
			Valid: true,
			Src1:  uint8(cycle % 64),
			Src2:  uint8((cycle + 1) % 64),
			Dest:  uint8((cycle + 2) % 64),
		})
	}

	// Verify state is consistent
	validCount := 0
	for i := 0; i < 32; i++ {
		if sched.Window.Ops[i].Valid {
			validCount++
		}
	}

	if validCount != 32 {
		t.Errorf("All slots should be valid, got %d", validCount)
	}
}

func TestStress_AllRegistersUsed(t *testing.T) {
	// WHAT: All 64 registers active
	// WHY: Maximum register pressure
	// CATEGORY: [STRESS]

	sched := NewOoOScheduler()

	// Mark all registers pending
	for i := uint8(0); i < 64; i++ {
		sched.Scoreboard.MarkPending(i)
	}

	// Verify all pending
	for i := uint8(0); i < 64; i++ {
		if sched.Scoreboard.IsReady(i) {
			t.Errorf("Register %d should be pending", i)
		}
	}

	// Mark all ready
	for i := uint8(0); i < 64; i++ {
		sched.Scoreboard.MarkReady(i)
	}

	// Verify all ready
	for i := uint8(0); i < 64; i++ {
		if !sched.Scoreboard.IsReady(i) {
			t.Errorf("Register %d should be ready", i)
		}
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 16. BYPASS INTEGRATION
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Full bypass forwarding integration tests.
// Verifies that back-to-back dependent operations work correctly.
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestBypassIntegration_BackToBack(t *testing.T) {
	// WHAT: Back-to-back dependent ops issue in consecutive cycles via bypass
	// WHY: This is the KEY test for single-cycle ALU forwarding
	// CATEGORY: [INTEGRATION] [BYPASS]

	sched := NewOoOScheduler()

	// Producer
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Issue producer
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 20 {
		t.Error("Should issue producer first")
	}

	// Now add consumer (reads R10 which producer just issued)
	// Consumer should be ready via bypass (R10 is pending in scoreboard but available via bypass)
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Verify bypass is available
	if !sched.CheckBypass(10) {
		t.Error("Bypass should be available for R10")
	}

	// Issue consumer via bypass
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()

	if bundle.Valid == 0 {
		t.Error("Consumer should issue via bypass")
	}
	if bundle.Indices[0] != 15 {
		t.Errorf("Should issue consumer (slot 15), got slot %d", bundle.Indices[0])
	}
}

func TestBypassIntegration_ChainWithBypass(t *testing.T) {
	// WHAT: Chain of 3 ops with bypass forwarding
	// WHY: Verify bypass works through multiple levels
	// CATEGORY: [INTEGRATION] [BYPASS]

	sched := NewOoOScheduler()

	// Enter all three ops
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12})

	// Issue A
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()
	if bundle.Indices[0] != 25 {
		t.Error("Should issue A")
	}

	// B should be ready via bypass (R10)
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()
	if bundle.Valid == 0 || bundle.Indices[0] != 20 {
		t.Error("B should issue via bypass")
	}

	// C should be ready via bypass (R11)
	sched.ScheduleCycle0()
	bundle = sched.ScheduleCycle1()
	if bundle.Valid == 0 || bundle.Indices[0] != 15 {
		t.Error("C should issue via bypass")
	}
}

func TestBypassIntegration_NoBypassAfterTwoCycles(t *testing.T) {
	// WHAT: Bypass only available for one cycle
	// WHY: Bypass represents in-flight result, not committed value
	// CATEGORY: [INTEGRATION] [BYPASS]

	sched := NewOoOScheduler()

	// Producer
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Issue producer
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Bypass available now
	if !sched.CheckBypass(10) {
		t.Error("Bypass should be available immediately after issue")
	}

	// Empty issue cycle (clears bypass)
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Bypass should be gone
	if sched.CheckBypass(10) {
		t.Error("Bypass should be cleared after one cycle")
	}
}

func TestBypassIntegration_ScoreboardTakesOver(t *testing.T) {
	// WHAT: After completion, scoreboard (not bypass) provides readiness
	// WHY: Completion updates scoreboard, which is the long-term source
	// CATEGORY: [INTEGRATION] [BYPASS]

	sched := NewOoOScheduler()

	// Producer
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Issue producer
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Complete producer
	sched.ScheduleComplete([IssueWidth]uint8{10}, 0b1)

	// Clear bypass (next issue cycle)
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Now add consumer
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Consumer should be ready via scoreboard (not bypass)
	if !sched.Scoreboard.IsReady(10) {
		t.Error("Scoreboard should show R10 ready after completion")
	}

	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Valid == 0 {
		t.Error("Consumer should be ready via scoreboard")
	}
}

func TestBypassIntegration_MultipleBypassSameRegister(t *testing.T) {
	// WHAT: Two instructions in same bundle write different registers
	// WHY: Both should create bypass entries
	// CATEGORY: [INTEGRATION] [BYPASS]

	sched := NewOoOScheduler()

	// Two independent producers
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(24, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11})

	// Issue both
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bits.OnesCount16(bundle.Valid) != 2 {
		t.Error("Both producers should issue")
	}

	// Both bypasses should be available
	if !sched.CheckBypass(10) || !sched.CheckBypass(11) {
		t.Error("Both bypasses should be available")
	}
}

func TestBypassIntegration_BypassExpiry(t *testing.T) {
	// WHAT: Verify bypass expires correctly after one cycle
	// WHY: Stale bypass would cause incorrect forwarding
	// CATEGORY: [INTEGRATION] [BYPASS]

	sched := NewOoOScheduler()

	// Producer
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Issue producer
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Consumer added (uses bypass)
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Consumer issues via bypass
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 20 {
		t.Error("Consumer should issue via bypass")
	}

	// Now bypass for R10 should be gone (replaced by R11)
	if sched.CheckBypass(10) {
		t.Error("Old bypass should be expired")
	}

	// New bypass for R11 should be available
	if !sched.CheckBypass(11) {
		t.Error("New bypass should be available")
	}
}

func TestBypassIntegration_NoBypassForPendingSourceFromOldIssue(t *testing.T) {
	// WHAT: Consumer added long after producer issued
	// WHY: Bypass only valid for one cycle after issue
	// CATEGORY: [INTEGRATION] [BYPASS]

	sched := NewOoOScheduler()

	// Producer
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Issue producer
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Several empty cycles (bypass expires)
	for i := 0; i < 5; i++ {
		sched.ScheduleCycle0()
		sched.ScheduleCycle1()
	}

	// Add consumer (no bypass available, R10 still pending)
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Consumer should be blocked (no bypass, scoreboard pending)
	bitmap := ComputeReadyBitmap(sched)

	if (bitmap>>20)&1 != 0 {
		t.Error("Consumer should be blocked (no bypass, pending source)")
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 17. STATE CONSISTENCY
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Verify state remains valid after various operations.
// These tests check that internal data structures stay consistent.
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestStateConsistency_AfterEnter(t *testing.T) {
	// WHAT: State consistent after entering instruction
	// WHY: All related structures updated atomically
	// CATEGORY: [INTEGRATION]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Window has instruction
	if !sched.Window.Ops[15].Valid {
		t.Error("Window should have valid instruction")
	}

	// UnissuedValid has bit set
	if (sched.UnissuedValid>>15)&1 != 1 {
		t.Error("UnissuedValid should have bit set")
	}

	// Scoreboard unchanged (dest still ready from init)
	if !sched.Scoreboard.IsReady(10) {
		t.Error("Scoreboard should not change on enter")
	}
}

func TestStateConsistency_AfterIssue(t *testing.T) {
	// WHAT: State consistent after issuing instruction
	// WHY: All related structures updated atomically
	// CATEGORY: [INTEGRATION]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Window still has instruction (valid but issued)
	if !sched.Window.Ops[15].Valid {
		t.Error("Window should still have instruction")
	}
	if !sched.Window.Ops[15].Issued {
		t.Error("Issued flag should be set")
	}

	// UnissuedValid has bit cleared
	if (sched.UnissuedValid>>15)&1 != 0 {
		t.Error("UnissuedValid should have bit cleared")
	}

	// Scoreboard has dest pending
	if sched.Scoreboard.IsReady(10) {
		t.Error("Scoreboard should have dest pending")
	}

	// Bypass available
	if !sched.CheckBypass(10) {
		t.Error("Bypass should be available")
	}
}

func TestStateConsistency_AfterComplete(t *testing.T) {
	// WHAT: State consistent after completion
	// WHY: Scoreboard updated correctly
	// CATEGORY: [INTEGRATION]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	sched.ScheduleComplete([IssueWidth]uint8{10}, 0b1)

	// Scoreboard has dest ready
	if !sched.Scoreboard.IsReady(10) {
		t.Error("Scoreboard should have dest ready after complete")
	}

	// Window still has instruction (until retire)
	if !sched.Window.Ops[15].Valid {
		t.Error("Window should still have instruction until retire")
	}
}

func TestStateConsistency_AfterRetire(t *testing.T) {
	// WHAT: State consistent after retire
	// WHY: All traces removed
	// CATEGORY: [INTEGRATION]

	sched := NewOoOScheduler()

	sched.EnterInstruction(15, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.RetireInstruction(15)

	// Window slot cleared
	if sched.Window.Ops[15].Valid {
		t.Error("Window should not have instruction after retire")
	}

	// UnissuedValid cleared
	if (sched.UnissuedValid>>15)&1 != 0 {
		t.Error("UnissuedValid should be cleared")
	}

	// Dep matrix row cleared
	if sched.DepMatrix[15] != 0 {
		t.Error("Dep matrix row should be cleared")
	}

	// Dep matrix column cleared
	for i := 0; i < 32; i++ {
		if (sched.DepMatrix[i]>>15)&1 != 0 {
			t.Errorf("Dep matrix column bit should be cleared in row %d", i)
		}
	}
}

func TestStateConsistency_WindowCompaction(t *testing.T) {
	// WHAT: Simulate retire-and-refill pattern
	// WHY: Real scheduler continuously retires and refills
	// CATEGORY: [INTEGRATION] [STRESS]

	sched := NewOoOScheduler()

	// Initial fill
	for i := 0; i < 32; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(i)})
	}

	// Issue all
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	// Complete and retire issued ops
	for i := 0; i < 16; i++ {
		if (bundle.Valid>>i)&1 != 0 {
			idx := bundle.Indices[i]
			dest := sched.Window.Ops[idx].Dest
			sched.ScheduleComplete([IssueWidth]uint8{dest}, 1<<i)
			sched.RetireInstruction(int(idx))
		}
	}

	// Refill retired slots
	for i := 0; i < 32; i++ {
		if !sched.Window.Ops[i].Valid {
			sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(32 + i)})
		}
	}

	// Should have 32 valid ops again
	count := 0
	for i := 0; i < 32; i++ {
		if sched.Window.Ops[i].Valid {
			count++
		}
	}
	if count != 32 {
		t.Errorf("Should have 32 valid ops after refill, got %d", count)
	}
}

func TestStateConsistency_DepMatrixSymmetry(t *testing.T) {
	// WHAT: Dependency matrix maintains proper structure
	// WHY: Matrix should only have producer→consumer edges
	// CATEGORY: [INTEGRATION]

	sched := NewOoOScheduler()

	// Create various dependencies
	sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 4, Dest: 12})
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 11, Src2: 12, Dest: 13})

	// Verify structure: edges only go from older to younger
	for i := 0; i < 32; i++ {
		for j := 0; j < 32; j++ {
			if (sched.DepMatrix[i]>>j)&1 == 1 {
				// Edge from i to j means i is producer, j is consumer
				// Producer must be older (higher slot)
				if i <= j {
					t.Errorf("Invalid edge: %d→%d (producer must be older)", i, j)
				}
			}
		}
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 18. REGRESSION TESTS
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Specific scenarios that could harbor bugs.
// These test potential failure modes discovered through analysis.
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestRegression_BypassWithPendingScoreboard(t *testing.T) {
	// WHAT: Bypass works even when scoreboard says pending
	// WHY: This is the core bypass functionality
	// CATEGORY: [REGRESSION]

	sched := NewOoOScheduler()

	// R10 pending in scoreboard, but available via bypass
	sched.Scoreboard.MarkPending(10)
	sched.LastIssuedDests[0] = 10
	sched.LastIssuedValid = 1

	// Consumer should see R10 as available
	sched.EnterInstruction(0, Operation{Valid: true, Src1: 10, Src2: 1, Dest: 11})

	bitmap := ComputeReadyBitmap(sched)

	if bitmap != 1 {
		t.Error("Consumer should be ready via bypass despite pending scoreboard")
	}
}

func TestRegression_WAWBlocksOnScoreboard(t *testing.T) {
	// WHAT: Younger WAW writer blocked by scoreboard, not just claimed mask
	// WHY: WAW must wait for older writer to complete
	// CATEGORY: [REGRESSION]

	sched := NewOoOScheduler()

	// Two writers to R10
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 10})

	// Issue older
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	if bundle.Indices[0] != 25 {
		t.Error("Should issue older first")
	}

	// Younger should be blocked by scoreboard (R10 pending)
	bitmap := ComputeReadyBitmap(sched)

	if (bitmap>>20)&1 != 0 {
		t.Error("Younger WAW should be blocked by pending dest")
	}
}

func TestRegression_UnissuedProducerCheck(t *testing.T) {
	// WHAT: Consumer blocked until producer issues (not just completes)
	// WHY: Incremental design requires UnissuedValid check
	// CATEGORY: [REGRESSION]

	sched := NewOoOScheduler()

	// Producer (unissued)
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Consumer
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Check that consumer is blocked by unissued producer
	bitmap := ComputeReadyBitmap(sched)

	if (bitmap>>20)&1 != 0 {
		t.Error("Consumer should be blocked by UNISSUED producer")
	}

	// Issue producer
	sched.ScheduleCycle0()
	sched.ScheduleCycle1()

	// Now consumer should be ready via bypass
	bitmap = ComputeReadyBitmap(sched)

	if (bitmap>>20)&1 != 1 {
		t.Error("Consumer should be ready after producer issues (bypass)")
	}
}

func TestRegression_DependencyOnIssuedNotUnissued(t *testing.T) {
	// WHAT: Dependency created with issued producer shouldn't happen
	// WHY: Only unissued instructions create blocking dependencies
	// CATEGORY: [REGRESSION]

	sched := NewOoOScheduler()

	// Producer - enter and issue
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.Window.Ops[25].Issued = true
	sched.UnissuedValid &= ^uint32(1 << 25)

	// Now enter consumer
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// No dependency should be created (producer already issued)
	if (sched.DepMatrix[25]>>20)&1 != 0 {
		t.Error("Should not create dependency with issued producer")
	}
}

func TestRegression_ClearedBypassDoesntMatch(t *testing.T) {
	// WHAT: After bypass clears, register doesn't match
	// WHY: Stale bypass would cause incorrect ready detection
	// CATEGORY: [REGRESSION]

	sched := NewOoOScheduler()

	// Set up bypass
	sched.LastIssuedDests[0] = 10
	sched.LastIssuedValid = 1

	// Clear bypass (empty issue)
	sched.UpdateAfterIssue(IssueBundle{Valid: 0})

	// Bypass should not match
	if sched.CheckBypass(10) {
		t.Error("Cleared bypass should not match")
	}
}

func TestRegression_RetireDoesntAffectOtherSlots(t *testing.T) {
	// WHAT: Retiring one slot doesn't affect others
	// WHY: Each slot is independent
	// CATEGORY: [REGRESSION]

	sched := NewOoOScheduler()

	// Enter two independent instructions
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
	sched.EnterInstruction(20, Operation{Valid: true, Src1: 3, Src2: 4, Dest: 11})

	// Retire one
	sched.RetireInstruction(25)

	// Other should be unaffected
	if !sched.Window.Ops[20].Valid {
		t.Error("Unrelated slot should remain valid")
	}
	if (sched.UnissuedValid>>20)&1 != 1 {
		t.Error("Unrelated slot should remain in UnissuedValid")
	}
}

func TestRegression_OutOfOrderEntryDependency(t *testing.T) {
	// WHAT: Dependency created when consumer enters before producer
	// WHY: Out-of-order insertion must handle both directions
	// CATEGORY: [REGRESSION]

	sched := NewOoOScheduler()

	// Consumer first (younger slot, reads R10)
	sched.EnterInstruction(15, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})

	// Producer second (older slot, writes R10)
	sched.EnterInstruction(25, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})

	// Dependency should exist (slot 15 depends on slot 25)
	if (sched.DepMatrix[25]>>15)&1 != 1 {
		t.Error("Dependency should be created on out-of-order entry")
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 19. DOCUMENTATION TESTS
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// These tests document design properties and constraints.
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func TestDoc_StructureSizes(t *testing.T) {
	// WHAT: Document structure sizes for hardware budgeting
	// WHY: RTL needs to know exact bit widths
	// CATEGORY: [DOCUMENTATION]

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
	// CATEGORY: [DOCUMENTATION]

	if WindowSize != 32 {
		t.Errorf("WindowSize should be 32, got %d", WindowSize)
	}
	if NumRegisters != 64 {
		t.Errorf("NumRegisters should be 64, got %d", NumRegisters)
	}
	if IssueWidth != 16 {
		t.Errorf("IssueWidth should be 16, got %d", IssueWidth)
	}

	// Issue width <= Window size
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
	// CATEGORY: [DOCUMENTATION]

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

func TestDoc_ArchitectureSummary(t *testing.T) {
	// WHAT: Document key architectural features
	// WHY: Single source of truth
	// CATEGORY: [DOCUMENTATION]

	t.Log("SUPRAX OoO SCHEDULER ARCHITECTURE:")
	t.Log("===================================")
	t.Log("")
	t.Log("INCREMENTAL DEPENDENCY MATRIX:")
	t.Log("  - Dependencies tracked as STATE, not recomputed each cycle")
	t.Log("  - Updated on instruction enter (UpdateDependenciesOnEnter)")
	t.Log("  - Cleared on instruction retire (UpdateDependenciesOnRetire)")
	t.Log("")
	t.Log("BYPASS FORWARDING:")
	t.Log("  - Single-cycle ALU forwarding for dependent chains")
	t.Log("  - LastIssuedDests + LastIssuedValid track bypass availability")
	t.Log("  - Reduces dependent chain latency from 4 cycles to 2 cycles")
	t.Log("")
	t.Log("UNISSUED VALID BITMAP:")
	t.Log("  - O(1) producer blocking check")
	t.Log("  - (dep_column & UnissuedValid) != 0 means blocked")
	t.Log("  - Replaces O(n) OR tree scan")
	t.Log("")
	t.Log("HAZARD HANDLING:")
	t.Log("  - RAW: Dep matrix + Scoreboard + Bypass")
	t.Log("  - WAR: Implicit in slot ordering")
	t.Log("  - WAW: scoreboard[dest] + claimed mask")
	t.Log("")
	t.Log("PRIORITY SCHEDULING:")
	t.Log("  - High priority: ops with dependents (critical path)")
	t.Log("  - Low priority: ops without dependents (leaves)")
	t.Log("  - Oldest-first within each priority tier")
}

func TestDoc_BitmapSizes(t *testing.T) {
	// WHAT: Verify bitmap sizes match constants
	// WHY: Type safety for hardware mapping
	// CATEGORY: [DOCUMENTATION]

	// Scoreboard is uint64 (64 bits for 64 registers)
	var sb Scoreboard
	if unsafe.Sizeof(sb) != 8 {
		t.Errorf("Scoreboard should be 8 bytes, got %d", unsafe.Sizeof(sb))
	}

	// UnissuedValid is uint32 (32 bits for 32 slots)
	var unissuedValid uint32
	if unsafe.Sizeof(unissuedValid) != 4 {
		t.Errorf("UnissuedValid should be 4 bytes, got %d", unsafe.Sizeof(unissuedValid))
	}

	// LastIssuedValid is uint16 (16 bits for 16 issue width)
	var lastIssuedValid uint16
	if unsafe.Sizeof(lastIssuedValid) != 2 {
		t.Errorf("LastIssuedValid should be 2 bytes, got %d", unsafe.Sizeof(lastIssuedValid))
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗
// 20. BENCHMARKS
// ╚═══════════════════════════════════════════════════════════════════════════════════════════╝
//
// Performance measurement for critical path functions.
// These benchmarks help identify optimization opportunities.
//
// ╔═══════════════════════════════════════════════════════════════════════════════════════════╗

func BenchmarkComputeReadyBitmap(b *testing.B) {
	sched := NewOoOScheduler()

	for i := 0; i < 32; i++ {
		sched.EnterInstruction(i, Operation{
			Valid: true,
			Src1:  uint8(i % 64),
			Src2:  uint8((i + 1) % 64),
			Dest:  uint8((i + 2) % 64),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ComputeReadyBitmap(sched)
	}
}

func BenchmarkComputeHasDependents(b *testing.B) {
	var depMatrix DependencyMatrix
	for i := 0; i < 32; i++ {
		depMatrix[i] = uint32(i * 31337)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ComputeHasDependents(depMatrix)
	}
}

func BenchmarkClassifyPriority(b *testing.B) {
	readyBitmap := uint32(0xFFFFFFFF)
	hasDependents := uint32(0x55555555)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ClassifyPriority(readyBitmap, hasDependents)
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

func BenchmarkEnterInstruction(b *testing.B) {
	sched := NewOoOScheduler()
	op := Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slot := i % 32
		sched.EnterInstruction(slot, op)
	}
}

func BenchmarkRetireInstruction(b *testing.B) {
	sched := NewOoOScheduler()

	// Pre-fill
	for i := 0; i < 32; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(i)})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slot := i % 32
		sched.RetireInstruction(slot)
		// Re-enter for next iteration
		sched.EnterInstruction(slot, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(slot)})
	}
}

func BenchmarkCheckBypass(b *testing.B) {
	sched := NewOoOScheduler()

	// Set up bypass
	for i := 0; i < 16; i++ {
		sched.LastIssuedDests[i] = uint8(i * 4)
	}
	sched.LastIssuedValid = 0xFFFF

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sched.CheckBypass(uint8(i % 64))
	}
}

func BenchmarkUpdateAfterIssue(b *testing.B) {
	sched := NewOoOScheduler()

	for i := 0; i < 16; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(10 + i)})
	}

	bundle := IssueBundle{Valid: 0xFFFF}
	for i := 0; i < 16; i++ {
		bundle.Indices[i] = uint8(i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sched.UpdateAfterIssue(bundle)

		// Reset for next iteration
		for j := 0; j < 16; j++ {
			sched.Window.Ops[j].Issued = false
			sched.UnissuedValid |= 1 << j
			sched.Scoreboard.MarkReady(uint8(10 + j))
		}
	}
}

func BenchmarkFullCycle(b *testing.B) {
	sched := NewOoOScheduler()

	for i := 0; i < 32; i++ {
		sched.EnterInstruction(i, Operation{
			Valid: true,
			Src1:  uint8(i % 64),
			Src2:  uint8((i + 1) % 64),
			Dest:  uint8((i + 2) % 64),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sched.ScheduleCycle0()
		bundle := sched.ScheduleCycle1()

		// Reset for next iteration
		for j := 0; j < 16; j++ {
			if (bundle.Valid>>j)&1 != 0 {
				sched.Window.Ops[bundle.Indices[j]].Issued = false
				sched.UnissuedValid |= 1 << bundle.Indices[j]
			}
		}
		sched.Scoreboard = ^Scoreboard(0)
		sched.LastIssuedValid = 0
	}
}

func BenchmarkScoreboardOperations(b *testing.B) {
	var sb Scoreboard

	b.Run("MarkReady", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sb.MarkReady(uint8(i % 64))
		}
	})

	b.Run("MarkPending", func(b *testing.B) {
		sb = ^Scoreboard(0)
		for i := 0; i < b.N; i++ {
			sb.MarkPending(uint8(i % 64))
		}
	})

	b.Run("IsReady", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sb.IsReady(uint8(i % 64))
		}
	})
}

func BenchmarkDependencyMatrixUpdate(b *testing.B) {
	sched := NewOoOScheduler()

	// Create some existing instructions
	for i := 16; i < 32; i++ {
		sched.EnterInstruction(i, Operation{Valid: true, Src1: 1, Src2: 2, Dest: uint8(i)})
	}

	op := Operation{Valid: true, Src1: 20, Src2: 25, Dest: 10}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slot := i % 16
		sched.UpdateDependenciesOnEnter(slot, op)
		sched.UpdateDependenciesOnRetire(slot)
	}
}
