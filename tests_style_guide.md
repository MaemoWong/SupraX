# SUPRAX Test Suite Style Guide

This guide enables exact replication of the testing style used in `ooo_test.go` for other SUPRAX modules.

---

## 1. FILE STRUCTURE

### 1.1 Header Template

Every test file MUST begin with this structure:

```go
package <module>

import (
	"math/bits"
	"testing"
	"unsafe"
)

// ╔═══════════════════════════════════════════════════════════════════════════╗
// SUPRAX <Module Name> - Test Suite
// ╚═══════════════════════════════════════════════════════════════════════════╝
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
// [2-3 paragraphs explaining the component's purpose, how it works,
//  and why it's architected this way. Use concrete examples.]
//
// KEY ARCHITECTURAL FEATURES TESTED:
// ──────────────────────────────────
//
// FEATURE 1 NAME:
//   [Description of feature]
//   [Why it's designed this way]
//   [Hardware implications]
//
// FEATURE 2 NAME:
//   [Description of feature]
//   [Why it's designed this way]
//   [Hardware implications]
//
// [Continue for all key features...]
//
// ╔═══════════════════════════════════════════════════════════════════════════╗
// TEST COVERAGE MATRIX
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// This test suite aims for 100% coverage of:
//   - All public functions and methods
//   - All code paths within functions
//   - All boundary conditions (min/max values, edge indices)
//   - All [domain-specific patterns, e.g., hazard types, dependency patterns]
//   - All state transitions (enter → process → complete → retire)
//   - All [feature-specific scenarios]
//   - All error conditions and edge cases
//
// COVERAGE CATEGORIES:
//   [UNIT]        Single function/component in isolation
//   [INTEGRATION] Multiple components working together
//   [INVARIANT]   Properties that must ALWAYS hold
//   [STRESS]      High-volume, repeated operations
//   [BOUNDARY]    Edge cases at limits of valid input
//   [PATTERN]     Real-world usage patterns
//   [LIFECYCLE]   Full object/data lifecycle
//   [REGRESSION]  Specific bug scenarios
//   [Add domain-specific categories as needed]
```

### 1.2 Section Organization Template

```go
// ╔═══════════════════════════════════════════════════════════════════════════╗
// TEST ORGANIZATION
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// Tests are organized to mirror the hardware components and data flow:
//
// 1. COMPONENT A TESTS
//    [Description of what Component A does]
//
// 2. COMPONENT B TESTS
//    [Description of what Component B does]
//
// 3. COMPONENT INTEGRATION
//    [How components work together]
//
// [Continue organizing by logical groups...]
//
// N. INVARIANTS
//    Properties that must ALWAYS hold
//
// N+1. STRESS TESTS
//      High-volume, repeated operations
//
// N+2. EDGE CASES
//      Boundary conditions, corner cases
//
// N+3. DOCUMENTATION TESTS
//      Verify assumptions, print specs
//
// N+4. BENCHMARKS
//      Performance measurement
```

---

## 2. TEST SECTION HEADERS

Use box-drawing characters for visual hierarchy:

```go
// ╔═══════════════════════════════════════════════════════════════════════════╗
// 1. COMPONENT NAME TESTS
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// [2-4 paragraphs explaining:]
//   - What this component does
//   - Why it's designed this way
//   - How it maps to hardware
//   - Key invariants or properties
//
// HARDWARE MAPPING:
//   - [Specific RTL structure, e.g., "64 flip-flops, one per register"]
//   - [Operations, e.g., "Read: 6-to-64 decoder → single bit select"]
//   - [Updates, e.g., "MarkReady: OR gate sets single bit"]
//
// INVARIANTS:
//   - [Property 1 that must always hold]
//   - [Property 2 that must always hold]
//
// ╔═══════════════════════════════════════════════════════════════════════════╗
```

**Example from ooo_test.go:**

```go
// ╔═══════════════════════════════════════════════════════════════════════════╗
// 1. SCOREBOARD TESTS
// ╚═══════════════════════════════════════════════════════════════════════════╝
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
// ╔═══════════════════════════════════════════════════════════════════════════╗
```

---

## 3. INDIVIDUAL TEST FORMAT

### 3.1 Test Header Template

Every test MUST have this comment structure:

```go
func Test<Component>_<Scenario>(t *testing.T) {
	// WHAT: [One sentence: what is being tested]
	// WHY: [One sentence: why this test matters / what it validates]
	// HARDWARE: [One sentence: hardware correspondence]
	// CATEGORY: [TAG1] [TAG2] [TAG3]
	//
	// [OPTIONAL: Additional context, examples, or diagrams]
	// [Use this space for complex scenarios that need explanation]
	//
	// EXAMPLE:
	//   [Concrete example if helpful]
	
	// ... test body ...
}
```

### 3.2 Complete Examples

**Simple test:**

```go
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
```

**Complex test with diagram:**

```go
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
```

**Invariant test:**

```go
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
```

---

## 4. COMMENT CONVENTIONS

### 4.1 Inline Comments

Comments should explain **WHY** and **WHAT IF**, not just **WHAT**:

```go
// ❌ BAD - Just describes the code
// Mark register pending
sched.Scoreboard.MarkPending(10)

// ✅ GOOD - Explains purpose and implications
// Dest becomes pending (blocks younger RAW dependent ops)
sched.Scoreboard.MarkPending(10)
```

```go
// ❌ BAD
// Check if ready
if (bitmap>>15)&1 != 1 {

// ✅ GOOD - Explains what ready means in this context
// Consumer should be ready (both sources available via bypass)
if (bitmap>>15)&1 != 1 {
```

### 4.2 Section Comments Within Tests

Use visual separators for multi-phase tests:

```go
func TestComplexScenario(t *testing.T) {
	// ... test header ...

	sched := NewOoOScheduler()

	// SETUP PHASE
	// Fill window with test pattern
	for i := 0; i < 10; i++ {
		sched.EnterInstruction(i, ...)
	}

	// EXECUTION PHASE
	// Issue first batch
	sched.ScheduleCycle0()
	bundle := sched.ScheduleCycle1()

	// VERIFICATION PHASE
	// Verify expected ops were selected
	if bits.OnesCount16(bundle.Valid) != 5 {
		t.Error(...)
	}
}
```

### 4.3 Visual Diagrams

Use ASCII art for complex relationships:

```go
// STRUCTURE:
//   Level 0: A, B, C, D (4 independent)
//   Level 1: E=A+B, F=C+D (2 ops, each depends on 2 from level 0)
//   Level 2: G=E+F (1 op, depends on both from level 1)
```

```go
// SCENARIO:
//   Slot 20 (older): reads R5
//   Slot 15 (younger): writes R5
// This is NOT a hazard because slot 20 (reader) is older and will
// read R5 before slot 15 (writer) can modify it.
```

---

## 5. ERROR MESSAGES

### 5.1 Error Message Format

All error messages should:
1. State what **should** be true
2. Include actual values (with hex for bitmaps)
3. Provide context

```go
// ❌ BAD - No context, no actual value
if !ready {
	t.Error("Not ready")
}

// ✅ GOOD - Clear expectation, context, actual value
if !ready {
	t.Error("Consumer should be ready after producer completes")
}

// ✅ BETTER - Includes actual values
if bitmap != expected {
	t.Errorf("Expected bitmap 0x%08X, got 0x%08X", expected, bitmap)
}

// ✅ BEST - Full context
if (sched.DepMatrix[25]>>20)&1 != 1 {
	t.Errorf("Consumer (slot 20) should depend on producer (slot 25), matrix[25]=0x%08X", 
		sched.DepMatrix[25])
}
```

### 5.2 Fatal vs Error

Use `Fatal` only for invariant violations or test setup failures:

```go
// Use Fatal for invariant violations
if bundle2.Valid != 0 {
	t.Fatal("INVARIANT VIOLATION: Issued instruction selected again!")
}

// Use Fatal for test setup failures
if bundle1.Valid != 1 {
	t.Fatal("First issue should succeed")  // Can't continue test
}

// Use Error for normal test failures
if sched.Scoreboard.IsReady(10) {
	t.Error("Dest should be pending after issue")  // Test can continue
}
```

---

## 6. TEST NAMING

### 6.1 Naming Convention

Format: `Test<Component>_<Scenario>`

Components:
- Use the actual struct/type name
- Or use category (Pattern, Hazard, Invariant, etc.)

Scenarios:
- Be specific and descriptive
- Include key aspects (what's being tested)
- Can be long if necessary

**Examples:**

```go
// Component-based
TestScoreboard_MarkReady_Single
TestScoreboard_AllRegisters
TestScoreboard_BoundaryRegisters

// Operation-based  
TestDepMatrix_EnterWithDependency
TestDepMatrix_RetireClearsRow
TestDepMatrix_EnterRAW_Src1

// Pattern-based
TestPattern_Forest
TestPattern_WideTree
TestPattern_DeepChain

// Category-based
TestHazard_RAW_Detected
TestHazard_WAW_BlockedByScoreboard
TestInvariant_NoDoubleIssue
TestEdge_Register0

// Integration-based
TestPipeline_BasicOperation
TestBypassIntegration_BackToBack
TestStateConsistency_AfterEnter
```

### 6.2 Naming Guidelines

✅ DO:
- Be descriptive
- Include the specific condition being tested
- Use underscores to separate logical parts
- Include category prefix when appropriate

❌ DON'T:
- Use generic names like `TestBasic`, `TestSimple`
- Use abbreviations unless universally known
- Mix naming styles within a file

---

## 7. TEST ORGANIZATION PRINCIPLES

### 7.1 Ordering Within Sections

Tests within each section should progress:

1. **Initial state / Constructor**
2. **Single operations** (basic positive cases)
3. **Multiple operations** (parallel, sequential)
4. **Boundary conditions** (0, max, edges)
5. **Special patterns** (idempotent, reversible, etc.)
6. **Error cases** (if applicable)

**Example:**

```go
// 1. Initial State
TestComponent_InitialEmpty
TestComponent_Constructor

// 2. Single Operations
TestComponent_OperationA_Single
TestComponent_OperationB_Single

// 3. Multiple Operations
TestComponent_OperationA_Multiple
TestComponent_CombinedOperations

// 4. Boundary Conditions
TestComponent_BoundaryValue0
TestComponent_BoundaryValueMax
TestComponent_AllSlots

// 5. Special Patterns
TestComponent_Idempotent
TestComponent_ReverseOrder
TestComponent_InterleavePattern

// 6. Error Cases (if applicable)
TestComponent_InvalidInput
```

### 7.2 Integration vs Unit

**Unit tests** (majority):
- Test single component/function
- Isolated from rest of system
- Fast, focused
- Tag: `[UNIT]`

**Integration tests**:
- Test multiple components together
- Realistic workflows
- End-to-end scenarios
- Tag: `[INTEGRATION]`

**Example progression:**

```go
// Unit test - isolated
func TestScoreboard_MarkReady_Single(t *testing.T) {
	// CATEGORY: [UNIT]
	var sb Scoreboard
	sb.MarkReady(5)
	// verify...
}

// Integration test - full scheduler
func TestPipeline_DependencyChain(t *testing.T) {
	// CATEGORY: [INTEGRATION] [PIPELINE] [PATTERN]
	sched := NewOoOScheduler()
	// enter chain of dependent ops
	// issue, complete through pipeline
	// verify order...
}
```

---

## 8. COVERAGE CATEGORIES

### 8.1 Standard Category Tags

Use these tags consistently:

```go
[UNIT]        // Single function/component in isolation
[INTEGRATION] // Multiple components working together
[INVARIANT]   // Properties that must ALWAYS hold
[STRESS]      // High-volume, repeated operations
[BOUNDARY]    // Edge cases at limits of valid input
[PATTERN]     // Real-world dependency/usage patterns
[LIFECYCLE]   // Full instruction/object lifecycle
[PIPELINE]    // Multi-stage pipeline behavior
[REGRESSION]  // Specific bug scenarios
[HAZARD]      // CPU hazard handling (if applicable)
```

### 8.2 Domain-Specific Categories

Add module-specific categories as needed:

```go
// For OoO Scheduler
[BYPASS]      // Forwarding/bypass network

// For Branch Predictor
[TRAINING]    // Predictor training behavior
[ALIASING]    // Index aliasing scenarios

// For Memory System
[COHERENCE]   // Cache coherence protocol
[ORDERING]    // Memory ordering rules
```

### 8.3 Multiple Categories

Tests can have multiple categories:

```go
// CATEGORY: [INTEGRATION] [PIPELINE] [PATTERN]
// CATEGORY: [UNIT] [BOUNDARY]
// CATEGORY: [STRESS] [INVARIANT]
```

---

## 9. HARDWARE CORRESPONDENCE

### 9.1 Always Mention Hardware

Every test header MUST include hardware mapping:

```go
// HARDWARE: [One sentence explaining RTL correspondence]
```

**Examples:**

```go
// HARDWARE: OR gate sets single bit: scoreboard |= (1 << reg)
// HARDWARE: All 1024 flip-flops reset to 0
// HARDWARE: 32 parallel ready checkers (one per slot)
// HARDWARE: XOR comparators find match, matrix bit set
// HARDWARE: AND + zero-detect
// HARDWARE: Comparator output ANDed with valid bit
```

### 9.2 Hardware Section Comments

For complex components, include detailed hardware mapping:

```go
// HARDWARE MAPPING:
//   - 64 flip-flops, one per architectural register
//   - Read: 6-to-64 decoder → single bit select
//   - MarkReady: OR gate sets single bit
//   - MarkPending: AND gate clears single bit
```

---

## 10. SPECIAL TEST TYPES

### 10.1 Invariant Tests

Invariants are properties that MUST always hold:

```go
func TestInvariant_<Property>(t *testing.T) {
	// INVARIANT: [Statement of property that must always hold]
	// WHY: [Consequence if violated]
	// CATEGORY: [INVARIANT]

	// ... test setup ...

	// Test the property
	if violatesInvariant {
		t.Fatal("INVARIANT VIOLATION: [specific violation]")
	}
}
```

**Example:**

```go
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
```

### 10.2 Boundary Tests

Test at the edges of valid ranges:

```go
func TestEdge_<BoundaryCondition>(t *testing.T) {
	// WHAT: [What boundary is being tested]
	// WHY: [Why this boundary might harbor bugs]
	// HARDWARE: [Hardware consideration at boundary]
	// CATEGORY: [BOUNDARY]

	// Test at boundary...
}
```

**Common boundaries:**
- Register 0, Register 63
- Slot 0, Slot 31
- Empty states (all zeros)
- Full states (all ones)
- Maximum capacity
- Overflow/underflow points

### 10.3 Pattern Tests

Test real-world dependency/usage patterns:

```go
func TestPattern_<PatternName>(t *testing.T) {
	// WHAT: [Description of pattern]
	// WHY: [Why this pattern is common/important]
	// CATEGORY: [PATTERN]
	//
	// STRUCTURE:
	//   [ASCII diagram or description of pattern]

	// Build pattern...
	// Execute...
	// Verify expected behavior...
}
```

**Example patterns:**
- Chain (A→B→C)
- Diamond (A→{B,C}→D)
- Forest (multiple independent trees)
- Wide tree (one root, many leaves)
- Reduction tree

### 10.4 Stress Tests

Test at high volume:

```go
func TestStress_<Scenario>(t *testing.T) {
	// WHAT: [High-volume scenario]
	// WHY: [What could break under stress]
	// CATEGORY: [STRESS]

	// Large number of operations...
	for i := 0; i < 1000; i++ {
		// ...
	}

	// Verify system remained stable...
}
```

### 10.5 Documentation Tests

Tests that document architecture:

```go
func TestDoc_<Aspect>(t *testing.T) {
	// WHAT: Document [specific aspect]
	// WHY: [Why this needs documentation]
	// CATEGORY: [DOCUMENTATION]

	// Log or verify architectural properties...
	t.Logf("Component size: %d bytes", unsafe.Sizeof(Component{}))
	
	// Verify design constraints...
	if WindowSize > 32 {
		t.Error("WindowSize must fit in uint32 bitmap")
	}
}
```

---

## 11. VERIFICATION PATTERNS

### 11.1 Bitmap Verification

For bitmaps, always check:
1. Expected bits are set
2. Unexpected bits are NOT set
3. Exact value matches (when appropriate)

```go
// Check specific bit
if (bitmap>>5)&1 != 1 {
	t.Error("Bit 5 should be set")
}

// Check bit is NOT set
if (bitmap>>5)&1 != 0 {
	t.Error("Bit 5 should NOT be set")
}

// Check exact value
expected := uint32(0b1010)
if bitmap != expected {
	t.Errorf("Expected 0x%08X, got 0x%08X", expected, bitmap)
}

// Check adjacent bits not affected
if bitmap&0xFFFFFFC0 != 0 {
	t.Error("High bits should not be affected")
}
```

### 11.2 State Verification

For state updates, verify:
1. Primary effect occurred
2. Related state updated consistently
3. Unrelated state unchanged

```go
// After issuing instruction:

// Primary effect
if !sched.Window.Ops[15].Issued {
	t.Error("Issued flag should be set")
}

// Related state
if sched.Scoreboard.IsReady(10) {
	t.Error("Dest should be pending")
}

// Unrelated state
if !sched.Window.Ops[20].Valid {
	t.Error("Unrelated slot should be unchanged")
}
```

### 11.3 Count Verification

When expecting specific counts:

```go
count := bits.OnesCount16(bundle.Valid)
if count != 3 {
	t.Errorf("Should select 3 ops, got %d", count)
}

// Or for specific range
if count < 1 || count > 16 {
	t.Errorf("Invalid count: %d (expected 1-16)", count)
}
```

### 11.4 Loop Verification

For batch verification:

```go
// Verify all items in set
for i := 0; i < 5; i++ {
	if !sched.Scoreboard.IsReady(uint8(10 + i)) {
		t.Errorf("Register %d should be ready", 10+i)
	}
}

// Verify none in set
for i := 0; i < 32; i++ {
	if sched.Window.Ops[i].Valid {
		t.Errorf("Slot %d should be invalid", i)
	}
}
```

---

## 12. BENCHMARKS

### 12.1 Benchmark Format

```go
func Benchmark<Operation>(b *testing.B) {
	// Setup (not timed)
	component := SetupComponent()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Operation being measured
		component.Operation()
	}
}
```

### 12.2 Benchmark Organization

Group related benchmarks:

```go
func BenchmarkComponentOperations(b *testing.B) {
	component := SetupComponent()

	b.Run("OperationA", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			component.OperationA()
		}
	})

	b.Run("OperationB", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			component.OperationB()
		}
	})
}
```

### 12.3 Full Benchmark Example

```go
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
```

---

## 13. FORMATTING CONVENTIONS

### 13.1 Hex Formatting

Always use appropriate hex formatting:

```go
// 8-bit values
t.Errorf("Expected 0x%02X, got 0x%02X", expected, actual)

// 16-bit values
t.Errorf("Expected 0x%04X, got 0x%04X", expected, actual)

// 32-bit values
t.Errorf("Expected 0x%08X, got 0x%08X", expected, actual)

// 64-bit values
t.Errorf("Expected 0x%016X, got 0x%016X", expected, actual)
```

### 13.2 Binary Literals

Use binary for bit patterns when it aids clarity:

```go
// Clear which bits are set
bundle := IssueBundle{Valid: 0b1010}

// Hex when pattern doesn't matter
bundle := IssueBundle{Valid: 0xFFFF}

// Mixed is fine when it makes sense
if sched.UnissuedValid != 0b11110000 {
	t.Errorf("Expected 0x%08X", sched.UnissuedValid)
}
```

### 13.3 Alignment

Align related assignments for readability:

```go
// Good alignment
sched.EnterInstruction(31, Operation{Valid: true, Src1: 1, Src2: 2, Dest: 10})
sched.EnterInstruction(25, Operation{Valid: true, Src1: 10, Src2: 3, Dest: 11})
sched.EnterInstruction(20, Operation{Valid: true, Src1: 11, Src2: 4, Dest: 12})

// Also good for arrays
expected := [4]uint8{10, 11, 12, 13}
actual   := [4]uint8{ 9, 11, 12, 13}
```

---

## 14. COMPLETE EXAMPLE: Writing Tests for a New Module

Let's say we're writing tests for a **Return Address Stack (RAS)** component.

### Step 1: File Header

```go
package ras

import (
	"testing"
	"unsafe"
)

// ╔═══════════════════════════════════════════════════════════════════════════╗
// SUPRAX Return Address Stack - Test Suite
// ╚═══════════════════════════════════════════════════════════════════════════╝
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
// The Return Address Stack (RAS) predicts function return addresses.
// When a CALL instruction executes, the RAS pushes the return address.
// When a RET instruction executes, the RAS pops and predicts that address.
//
// The RAS is a hardware stack with limited depth (typically 8-16 entries).
// Overflow/underflow must be handled gracefully without corrupting state.
//
// KEY ARCHITECTURAL FEATURES TESTED:
// ──────────────────────────────────
//
// STACK OPERATIONS:
//   Push adds return address to top of stack.
//   Pop removes and returns top entry.
//   Peek reads top without removing (for speculation).
//
// OVERFLOW HANDLING:
//   Push when full: oldest entry evicted (circular buffer behavior).
//   Ensures stack never overflows, at cost of losing oldest data.
//
// UNDERFLOW HANDLING:
//   Pop when empty: returns zero/invalid address.
//   Prevents corruption, prediction fails gracefully.
//
// SPECULATION SUPPORT:
//   Peek allows prediction without committing.
//   Rollback restores state on misprediction.
//
// ╔═══════════════════════════════════════════════════════════════════════════╗
// TEST COVERAGE MATRIX
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// This test suite aims for 100% coverage of:
//   - All public functions (Push, Pop, Peek, Rollback)
//   - All code paths within functions
//   - All boundary conditions (empty, full, overflow, underflow)
//   - All usage patterns (nested calls, recursive functions)
//   - All state transitions
//   - All error conditions and edge cases
//
// COVERAGE CATEGORIES:
//   [UNIT]        Single function/component in isolation
//   [INTEGRATION] Multiple components working together
//   [INVARIANT]   Properties that must ALWAYS hold
//   [STRESS]      High-volume, repeated operations
//   [BOUNDARY]    Edge cases at limits of valid input
//   [PATTERN]     Real-world usage patterns
//   [LIFECYCLE]   Full operation lifecycle
//   [REGRESSION]  Specific bug scenarios

// ╔═══════════════════════════════════════════════════════════════════════════╗
// TEST ORGANIZATION
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// Tests are organized to mirror the hardware operations:
//
// 1. CONSTRUCTOR & INITIAL STATE
//    Verify RAS initializes correctly
//
// 2. BASIC STACK OPERATIONS
//    Push, Pop, Peek in isolation
//
// 3. STACK DEPTH MANAGEMENT
//    Verify depth counter updates correctly
//
// 4. OVERFLOW HANDLING
//    Push beyond capacity
//
// 5. UNDERFLOW HANDLING
//    Pop from empty stack
//
// 6. USAGE PATTERNS
//    Nested calls, recursive patterns
//
// 7. INVARIANTS
//    Properties that must always hold
//
// 8. EDGE CASES
//    Boundary conditions
//
// 9. STRESS TESTS
//    High-volume operations
//
// 10. DOCUMENTATION
//     Architecture verification
//
// 11. BENCHMARKS
//     Performance measurement
```

### Step 2: Section 1 - Basic Tests

```go
// ╔═══════════════════════════════════════════════════════════════════════════╗
// 1. CONSTRUCTOR & INITIAL STATE
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// The RAS starts empty with depth counter at 0.
// All stack entries should be initialized to zero.
//
// HARDWARE MAPPING:
//   - Depth counter: log2(StackSize) bit register
//   - Stack storage: StackSize × AddressWidth flip-flops
//   - All registers reset to 0 on initialization
//
// INVARIANTS:
//   - 0 ≤ depth ≤ StackSize
//   - Depth = 0 means stack is empty
//   - Depth = StackSize means stack is full
//
// ╔═══════════════════════════════════════════════════════════════════════════╗

func TestRAS_InitialEmpty(t *testing.T) {
	// WHAT: Fresh RAS starts empty
	// WHY: Verify zero-initialization
	// HARDWARE: All registers reset to 0
	// CATEGORY: [UNIT]

	ras := NewRAS(8)

	if !ras.IsEmpty() {
		t.Error("Fresh RAS should be empty")
	}

	if ras.Depth() != 0 {
		t.Errorf("Fresh RAS depth should be 0, got %d", ras.Depth())
	}
}

func TestRAS_InitialNotFull(t *testing.T) {
	// WHAT: Fresh RAS is not full
	// WHY: Verify initial capacity
	// HARDWARE: Depth counter = 0, full flag = 0
	// CATEGORY: [UNIT]

	ras := NewRAS(8)

	if ras.IsFull() {
		t.Error("Fresh RAS should not be full")
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════╗
// 2. BASIC STACK OPERATIONS
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// Push, Pop, and Peek are the core stack operations.
//
// HARDWARE MAPPING:
//   Push: Increment depth, write to stack[depth]
//   Pop:  Read from stack[depth-1], decrement depth
//   Peek: Read from stack[depth-1], no state change
//
// ╔═══════════════════════════════════════════════════════════════════════════╗

func TestRAS_PushSingle(t *testing.T) {
	// WHAT: Push single address, verify state update
	// WHY: Basic push functionality
	// HARDWARE: Depth++, stack[depth] = addr
	// CATEGORY: [UNIT]

	ras := NewRAS(8)

	ras.Push(0x1000)

	if ras.IsEmpty() {
		t.Error("RAS should not be empty after push")
	}

	if ras.Depth() != 1 {
		t.Errorf("Depth should be 1, got %d", ras.Depth())
	}
}

func TestRAS_PopSingle(t *testing.T) {
	// WHAT: Push then pop, verify value and state
	// WHY: Basic pop functionality
	// HARDWARE: addr = stack[depth-1], depth--
	// CATEGORY: [UNIT]

	ras := NewRAS(8)

	ras.Push(0x1000)
	addr, valid := ras.Pop()

	if !valid {
		t.Error("Pop should be valid after push")
	}

	if addr != 0x1000 {
		t.Errorf("Expected 0x1000, got 0x%X", addr)
	}

	if !ras.IsEmpty() {
		t.Error("RAS should be empty after pop")
	}
}

func TestRAS_PeekNoStateChange(t *testing.T) {
	// WHAT: Peek reads top without changing state
	// WHY: Speculation requires non-destructive read
	// HARDWARE: Read stack[depth-1], no register updates
	// CATEGORY: [UNIT]

	ras := NewRAS(8)

	ras.Push(0x1000)
	
	addr, valid := ras.Peek()

	if !valid || addr != 0x1000 {
		t.Errorf("Peek should return 0x1000, got 0x%X valid=%v", addr, valid)
	}

	// State unchanged
	if ras.Depth() != 1 {
		t.Error("Peek should not change depth")
	}

	// Second peek returns same value
	addr2, valid2 := ras.Peek()
	if addr2 != addr || valid2 != valid {
		t.Error("Consecutive peeks should return same value")
	}
}

// ... continue with more tests ...
```

### Step 3: Pattern Test Example

```go
func TestPattern_NestedCalls(t *testing.T) {
	// WHAT: Nested function call pattern
	// WHY: Common in real code
	// CATEGORY: [PATTERN]
	//
	// STRUCTURE:
	//   main() calls A()
	//   A() calls B()
	//   B() calls C()
	//   Returns: C→B→A→main
	//
	// EXPECTED RAS STATE:
	//   After A call:  [ret_A]
	//   After B call:  [ret_A, ret_B]
	//   After C call:  [ret_A, ret_B, ret_C]
	//   After C ret:   [ret_A, ret_B]
	//   After B ret:   [ret_A]
	//   After A ret:   []

	ras := NewRAS(8)

	// main → A
	ras.Push(0x1000) // return to main
	
	// A → B
	ras.Push(0x2000) // return to A
	
	// B → C
	ras.Push(0x3000) // return to B

	// Verify stack depth
	if ras.Depth() != 3 {
		t.Errorf("Depth should be 3, got %d", ras.Depth())
	}

	// C returns to B
	addr, valid := ras.Pop()
	if !valid || addr != 0x3000 {
		t.Errorf("First return should be 0x3000, got 0x%X", addr)
	}

	// B returns to A
	addr, valid = ras.Pop()
	if !valid || addr != 0x2000 {
		t.Errorf("Second return should be 0x2000, got 0x%X", addr)
	}

	// A returns to main
	addr, valid = ras.Pop()
	if !valid || addr != 0x1000 {
		t.Errorf("Third return should be 0x1000, got 0x%X", addr)
	}

	// Stack should be empty
	if !ras.IsEmpty() {
		t.Error("Stack should be empty after all returns")
	}
}
```

### Step 4: Invariant Test Example

```go
func TestInvariant_DepthBounds(t *testing.T) {
	// INVARIANT: 0 ≤ depth ≤ StackSize always holds
	// WHY: Out-of-bounds depth corrupts state
	// CATEGORY: [INVARIANT]

	ras := NewRAS(8)

	// Test throughout various operations
	for i := 0; i < 20; i++ {
		ras.Push(uint64(0x1000 + i*0x100))
		
		depth := ras.Depth()
		if depth < 0 || depth > 8 {
			t.Fatalf("INVARIANT VIOLATION: depth %d out of bounds [0,8]", depth)
		}
	}

	for i := 0; i < 20; i++ {
		ras.Pop()
		
		depth := ras.Depth()
		if depth < 0 || depth > 8 {
			t.Fatalf("INVARIANT VIOLATION: depth %d out of bounds [0,8]", depth)
		}
	}
}
```

---

## 15. CHECKLIST: Before Committing Tests

Use this checklist to verify test quality:

### File Structure
- [ ] File has complete header with philosophy, coverage matrix, organization
- [ ] All sections have box-drawing headers
- [ ] Tests organized into logical sections
- [ ] Benchmarks at end of file

### Test Quality
- [ ] Every test has WHAT/WHY/HARDWARE/CATEGORY comment
- [ ] Test names are descriptive and follow convention
- [ ] All error messages explain what **should** be true
- [ ] Hex values formatted consistently (0x%08X, etc.)
- [ ] No magic numbers without explanation

### Coverage
- [ ] Unit tests for each public function
- [ ] Boundary tests (0, max, empty, full)
- [ ] Integration tests for workflows
- [ ] Invariant tests for critical properties
- [ ] Pattern tests for common usage
- [ ] Stress tests for high volume

### Hardware Correspondence
- [ ] Every test mentions hardware mapping
- [ ] Complex components have detailed hardware sections
- [ ] RTL implications are clear

### Code Quality
- [ ] No commented-out code
- [ ] No debug prints (except in documentation tests)
- [ ] Consistent formatting (run gofmt)
- [ ] Tests are independent (can run in any order)

---

## 16. ANTI-PATTERNS TO AVOID

### ❌ Vague Test Names
```go
// BAD
func TestBasic(t *testing.T)
func TestCase1(t *testing.T)
func TestStuff(t *testing.T)

// GOOD
func TestScoreboard_MarkReady_Single(t *testing.T)
func TestPattern_NestedCalls(t *testing.T)
```

### ❌ Missing Context in Errors
```go
// BAD
if !valid {
	t.Error("Invalid")
}

// GOOD
if !valid {
	t.Error("Pop should be valid after push")
}
```

### ❌ No Hardware Comment
```go
// BAD
func TestSomething(t *testing.T) {
	// WHAT: Tests something
	// WHY: Because it's important
	// CATEGORY: [UNIT]
	
// GOOD  
func TestSomething(t *testing.T) {
	// WHAT: Tests something
	// WHY: Because it's important
	// HARDWARE: Flip-flop update via AND gate
	// CATEGORY: [UNIT]
```

### ❌ Tests Depend on Each Other
```go
// BAD - depends on order
var globalState *Component

func TestA(t *testing.T) {
	globalState = New()
}

func TestB(t *testing.T) {
	globalState.DoSomething() // Fails if TestA didn't run
}

// GOOD - independent
func TestA(t *testing.T) {
	state := New()
	// ...
}

func TestB(t *testing.T) {
	state := New()
	// ...
}
```

### ❌ Too Much or Too Little Detail
```go
// BAD - too little
func Test(t *testing.T) {
	x.Do()
	if !x.Ok() {
		t.Error("bad")
	}
}

// BAD - too much (code speaks for itself)
func Test(t *testing.T) {
	// Create a new scheduler
	sched := NewScheduler()
	// Mark register 5 as ready by calling MarkReady
	sched.Scoreboard.MarkReady(5)
	// Now check if it's ready with IsReady
	if !sched.Scoreboard.IsReady(5) {
		t.Error("not ready")
	}
}

// GOOD - right level
func Test(t *testing.T) {
	sched := NewScheduler()
	sched.Scoreboard.MarkReady(5)
	
	// Register should be ready after MarkReady
	if !sched.Scoreboard.IsReady(5) {
		t.Error("Register 5 should be ready after MarkReady")
	}
}
```

---

## SUMMARY

This style guide captures the essence of `ooo_test.go`:

1. **Comprehensive headers** explaining philosophy, coverage, and organization
2. **Detailed test comments** with WHAT/WHY/HARDWARE/CATEGORY
3. **Clear error messages** stating expectations with context
4. **Hardware correspondence** in every test
5. **Systematic coverage** using category tags
6. **Progressive organization** from simple to complex
7. **Real-world patterns** alongside unit tests
8. **Invariant verification** for critical properties
9. **Consistent formatting** and naming conventions
10. **Documentation tests** capturing architecture

Follow this guide to create test suites that serve as both verification and specification for hardware implementation.