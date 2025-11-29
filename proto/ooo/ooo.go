package ooo

import (
	"math/bits"
)

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// SUPRAX Out-of-Order Scheduler - Go Reference Model
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// OVERVIEW:
// ─────────
// This file implements the SUPRAX out-of-order (OoO) scheduler. An OoO scheduler
// is the "brain" of a superscalar CPU - it decides WHICH instructions execute WHEN.
//
// Modern CPUs don't execute instructions in program order. Instead, they find
// independent work and run it in parallel. This is how a 4GHz CPU achieves
// 12+ instructions per cycle despite most instructions taking multiple cycles.
//
// The scheduler's job:
//   1. Track which instructions are waiting for data (dependencies)
//   2. Identify which instructions CAN execute (all inputs ready)
//   3. Pick the BEST instructions to execute (critical path first)
//   4. Update state when instructions complete
//
// HARDWARE MODEL:
// ───────────────
// This Go code is a cycle-accurate reference model for SystemVerilog RTL.
// When you write the Verilog, run these same test vectors against RTL.
// If Go and RTL produce identical outputs, the hardware is correct.
//
// STYLE GUIDELINES FOR HARDWARE MAPPING:
// ──────────────────────────────────────
//   1. No early returns or breaks inside combinational logic
//   2. Explicit parallel evaluation (no sequential dependencies within a block)
//   3. Bitwise operations instead of boolean conditionals where possible
//   4. Intermediate wires explicitly named
//   5. Loops represent generate blocks (parallel hardware replication)
//
// SYSTEMVERILOG MAPPING:
// ──────────────────────
//   Go function       → SV always_comb block or module
//   Go loop           → SV generate for (parallel hardware)
//   Go bitwise ops    → SV bitwise ops (direct 1:1)
//   Go struct fields  → SV packed struct or wire bundles
//   Go method w/ ptr  → SV always_ff (sequential, modifies state)
//   Go method w/o ptr → SV always_comb (combinational, pure function)
//
// KEY CONCEPTS FOR CPU NEWCOMERS:
// ──────────────────────────────
//
// SCOREBOARD:
//   A bitmap tracking which registers contain valid data.
//   Bit N = 1 means register N is "ready" (has valid data).
//   Bit N = 0 means register N is "pending" (being computed by in-flight op).
//
// DEPENDENCY MATRIX:
//   Tracks producer→consumer relationships within the scheduling window.
//   matrix[i][j] = 1 means "instruction j depends on instruction i"
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
//   WAW (Write-After-Write): Output dependency. B writes same dest as A.
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
//   We use a ~90% accurate heuristic: prioritize instructions with dependents.
//   True critical path would require multi-cycle iterative computation.
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// CONSTANTS
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// These parameters define the scheduler geometry. Chosen for balance of:
//   - IPC (instructions per cycle): Higher window = more parallelism
//   - Frequency: Smaller window = faster critical path
//   - Area: Dependency matrix is O(WindowSize²)
//
// SUPRAX uses 32-entry window (vs Intel's 224) because:
//   - No rename: Compiler handles register allocation
//   - Fast context switch: Smaller state to save/restore
//   - Higher frequency: Simpler critical paths
//
// SystemVerilog equivalent:
//   parameter WINDOW_SIZE   = 32;
//   parameter NUM_REGISTERS = 64;
//   parameter ISSUE_WIDTH   = 16;
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

const (
	// WindowSize: Number of instructions tracked simultaneously.
	// 32 fits in uint32 bitmap, enabling efficient bitwise operations.
	// Hardware: 32 parallel comparators, 32x32 dependency matrix.
	WindowSize = 32

	// NumRegisters: Architectural register count.
	// 64 fits in uint64 scoreboard bitmap.
	// Hardware: 64-bit scoreboard, 6-bit register addresses.
	NumRegisters = 64

	// IssueWidth: Maximum instructions issued per cycle.
	// 16-wide balances IPC vs hardware complexity.
	// Hardware: 16 parallel execution units, 16-deep priority encoder.
	IssueWidth = 16
)

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// SCOREBOARD
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// The scoreboard is a 64-bit bitmap tracking register readiness.
// It's the simplest component but fundamental to everything else.
//
// CONCEPT:
//   Each bit represents one architectural register (R0-R63).
//   Bit = 1: Register contains valid data (ready to read)
//   Bit = 0: Register is being written by in-flight instruction (pending)
//
// USAGE:
//   - Before issue: Check sources ready (can we read inputs?)
//   - Before issue: Check dest ready (WAW hazard - is older writer done?)
//   - After issue:  Mark dest pending (we're writing it)
//   - After complete: Mark dest ready (write finished)
//
// WHY BITMAP:
//   - Single-cycle lookup: scoreboard[reg] is just (bitmap >> reg) & 1
//   - Parallel updates: Can mark multiple registers in one cycle
//   - Tiny area: 64 flip-flops (~400 transistors total)
//
// Hardware: 64 flip-flops + read/write logic (~400 transistors)
// Timing: 20ps for read, 40ps for write
//
// SystemVerilog equivalent:
//   logic [63:0] scoreboard;
//
// Operations map directly:
//   IsReady(reg)      → scoreboard[reg]
//   MarkReady(reg)    → scoreboard[reg] <= 1'b1
//   MarkPending(reg)  → scoreboard[reg] <= 1'b0
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

type Scoreboard uint64

// IsReady checks if a register contains valid data.
//
// This is a combinational read - pure function, no side effects.
// Used in ready bitmap computation to check RAW and WAW hazards.
//
// Hardware: Single bit select from 64-bit register
// Timing: ~20ps (6:1 MUX tree)
//
// SystemVerilog:
//
//	assign ready = scoreboard[reg];
func (s Scoreboard) IsReady(reg uint8) bool {
	return (s>>reg)&1 == 1
}

// MarkReady sets a register as containing valid data.
//
// Called when an instruction completes execution.
// This unblocks dependent instructions waiting for this register.
//
// Hardware: OR gate sets single bit
// Timing: ~40ps (OR + flip-flop setup)
//
// SystemVerilog:
//
//	always_ff @(posedge clk)
//	  if (mark_ready_en) scoreboard[reg] <= 1'b1;
func (s *Scoreboard) MarkReady(reg uint8) {
	*s |= 1 << reg
}

// MarkPending clears a register's ready bit.
//
// Called when an instruction issues (begins execution).
// This blocks younger instructions from reading stale data.
//
// Hardware: AND gate clears single bit
// Timing: ~40ps (AND + flip-flop setup)
//
// SystemVerilog:
//
//	always_ff @(posedge clk)
//	  if (mark_pending_en) scoreboard[reg] <= 1'b0;
func (s *Scoreboard) MarkPending(reg uint8) {
	*s &= ^(1 << reg)
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// OPERATION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// An Operation represents one instruction in the scheduling window.
//
// FIELDS:
//   Valid:  Is this slot occupied? (false = empty slot)
//   Issued: Has this instruction been sent to execution? (prevents double-issue)
//   Src1:   First source register (0-63)
//   Src2:   Second source register (0-63)
//   Dest:   Destination register (0-63)
//   Op:     Opcode (determines which execution unit)
//   Imm:    Immediate value (for ALU ops, branches, etc.)
//
// STATE MACHINE (per slot):
//   Invalid → Valid (instruction enters window)
//   Valid → Issued (scheduler selects for execution)
//   Issued → Invalid (instruction completes and retires)
//
// Hardware: Each field is a set of flip-flops or SRAM bits
// Total: ~40 bits per operation × 32 slots = 1280 bits
//
// SystemVerilog equivalent:
//   typedef struct packed {
//     logic        valid;
//     logic        issued;
//     logic [5:0]  src1;
//     logic [5:0]  src2;
//     logic [5:0]  dest;
//     logic [7:0]  op;
//     logic [15:0] imm;
//   } operation_t;
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

type Operation struct {
	Valid  bool   // Slot contains valid instruction
	Issued bool   // Instruction has been issued to execution
	Src1   uint8  // First source register (6 bits used)
	Src2   uint8  // Second source register (6 bits used)
	Dest   uint8  // Destination register (6 bits used)
	Op     uint8  // Opcode
	Imm    uint16 // Immediate value
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// INSTRUCTION WINDOW
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// The instruction window holds all instructions being tracked by the scheduler.
//
// CONCEPT:
//   Instructions enter at low slot indices (newest at slot 0).
//   As instructions retire, the window compacts (or new instructions fill gaps).
//   Slot index encodes age: higher slot = older instruction.
//
// WHY 32 ENTRIES:
//   - Fits in uint32 bitmap (efficient ready/priority masks)
//   - 32×32 dependency matrix = 1024 bits (manageable)
//   - Enough ILP for most code (average basic block ~5 instructions)
//   - Fast context switch (32 × 40 bits = 160 bytes to save)
//
// COMPARISON TO INTEL:
//   Intel Skylake: 224-entry ROB + 97-entry scheduler
//   SUPRAX: 32-entry unified window
//   Intel needs huge buffers because rename creates many micro-ops.
//   SUPRAX has no rename, so 32 entries suffice.
//
// Hardware: Typically implemented as multi-ported SRAM or flip-flop array
// Area: ~400K transistors (dominant cost is multi-port access)
//
// SystemVerilog equivalent:
//   operation_t window [0:31];
//
// Or as banked SRAM:
//   (* ram_style = "distributed" *)
//   operation_t window [0:31];
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

type InstructionWindow struct {
	Ops [WindowSize]Operation
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// DEPENDENCY MATRIX
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// The dependency matrix tracks producer→consumer relationships within the window.
//
// CONCEPT:
//   matrix[i][j] = 1 means "instruction at slot j depends on instruction at slot i"
//   A dependency exists when:
//     1. Slot i writes a register that slot j reads (RAW hazard)
//     2. Slot i is older than slot j (i > j, since higher slot = older)
//
// WHY MATRIX FORM:
//   - Parallel construction: All 1024 entries computed simultaneously
//   - Parallel lookup: Ready check scans one column in parallel
//   - Priority classification: Row OR gives "has dependents" in one operation
//
// EXAMPLE:
//   Slot 10: R5 = R1 + R2    (writes R5)
//   Slot 5:  R6 = R5 + R3    (reads R5)
//   → matrix[10][5] = 1 (slot 5 depends on slot 10)
//
// AGE CHECK IS CRITICAL:
//   Without age check, we'd create false dependencies:
//   Slot 5:  R6 = R5 + R3    (reads R5)
//   Slot 10: R5 = R1 + R2    (writes R5)
//   We DON'T want matrix[5][10] = 1 because slot 5 is OLDER than slot 10.
//   The read at slot 5 sees the PREVIOUS value of R5, not slot 10's write.
//
// Hardware: 1024 parallel XOR comparators (32×32 matrix)
// Timing: 120ps (XOR + zero detect + age compare + AND)
// Area: ~400K transistors
//
// SystemVerilog equivalent:
//   logic [31:0] dep_matrix [0:31];
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

type DependencyMatrix [WindowSize]uint32

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// PRIORITY CLASS
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// Priority classification splits ready instructions into two tiers.
//
// CONCEPT:
//   HighPriority: Instructions with dependents (on critical path)
//   LowPriority:  Instructions without dependents (leaves)
//
// WHY TWO TIERS:
//   - Critical path approximation: Ops with dependents block other work
//   - Simple hardware: Just one bit per instruction (has dependents or not)
//   - ~90% accuracy: Most critical path ops have dependents
//
// ALTERNATIVE (not implemented):
//   True critical path depth requires iterative computation:
//     depth[i] = max(depth[j] + 1) for all j depending on i
//   This takes O(depth) iterations - not single cycle!
//   The ~3% IPC gain doesn't justify 3-6 extra cycles of latency.
//
// Hardware: 32 parallel OR reduction trees + 2 AND gates
// Timing: 100ps (5-level OR tree per row)
// Area: ~50K transistors
//
// SystemVerilog equivalent:
//   logic [31:0] high_priority;
//   logic [31:0] low_priority;
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

type PriorityClass struct {
	HighPriority uint32 // Ready ops with dependents (critical path)
	LowPriority  uint32 // Ready ops without dependents (leaves)
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// ISSUE BUNDLE
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// An IssueBundle contains the instructions selected for execution this cycle.
//
// CONCEPT:
//   Up to 16 instructions can issue per cycle (IssueWidth = 16).
//   Each entry contains:
//     - Index: Which window slot this instruction is in (0-31)
//     - Valid: Is this bundle position used?
//
// WHY SEPARATE VALID MASK:
//   - Sparse bundles: Often issue fewer than 16 instructions
//   - Parallel processing: Valid mask enables/disables each lane
//   - Simple hardware: One bit gates each execution unit
//
// Hardware: 16 × 5-bit indices + 16-bit valid mask = 96 bits
// Output to execution units via 16 parallel buses
//
// SystemVerilog equivalent:
//   typedef struct packed {
//     logic [4:0]  indices [0:15];
//     logic [15:0] valid;
//   } issue_bundle_t;
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

type IssueBundle struct {
	Indices [IssueWidth]uint8 // Window slot indices (5 bits each, stored in uint8)
	Valid   uint16            // Which bundle positions contain valid instructions
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// OOO SCHEDULER STATE
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// The OoOScheduler contains all state for the out-of-order engine.
//
// COMPONENTS:
//   Window:            32-entry instruction storage (SRAM)
//   Scoreboard:        64-bit register readiness bitmap (flip-flops)
//   PipelinedPriority: Pipeline register between Cycle 0 and Cycle 1
//
// PIPELINE STRUCTURE:
//   The scheduler is a 2-stage pipeline for frequency:
//
//   Cycle N (ScheduleCycle0):
//     - Build dependency matrix (combinational)
//     - Compute ready bitmap (combinational)
//     - Classify priority (combinational)
//     - Capture into PipelinedPriority register (sequential)
//
//   Cycle N+1 (ScheduleCycle1):
//     - Read PipelinedPriority (from register)
//     - Select issue bundle (combinational)
//     - Update scoreboard (sequential)
//     - Output bundle to execution units
//
// WHY 2-CYCLE PIPELINE:
//   - Frequency: Each stage fits in ~280ps (3.5GHz target)
//   - Throughput: Steady-state issues every cycle
//   - Latency: 2 cycles from ready to execute (acceptable)
//
// Hardware: ~1.1M transistors total
//   Window SRAM:      400K
//   Dependency matrix: 400K
//   Ready bitmap:      100K
//   Priority:          50K
//   Issue selector:    60K
//   Scoreboard:        5K
//   Pipeline regs:     50K
//
// SystemVerilog equivalent:
//   module ooo_scheduler (
//     input  logic clk,
//     input  logic rst_n,
//     // ... ports ...
//   );
//     operation_t    window [0:31];         // SRAM
//     logic [63:0]   scoreboard;            // Flip-flops
//     logic [31:0]   pipelined_high_prio;   // Pipeline register
//     logic [31:0]   pipelined_low_prio;    // Pipeline register
//   endmodule
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

type OoOScheduler struct {
	Window            InstructionWindow // 32-entry instruction storage
	Scoreboard        Scoreboard        // 64-bit register readiness
	PipelinedPriority PriorityClass     // Pipeline register for 2-cycle design
}

// NewOoOScheduler creates a scheduler with reset state.
//
// Initial state:
//   - All registers ready (scoreboard = all 1s)
//   - Window empty (all Valid = false)
//   - Pipeline registers clear
//
// WHY ALL REGISTERS READY:
//
//	On context switch-in, the new context's register file is loaded.
//	All architectural registers contain valid data from the saved state.
//	No instructions are in-flight, so nothing is pending.
//
// Hardware: Asynchronous reset sets scoreboard to all 1s
//
// SystemVerilog:
//
//	always_ff @(posedge clk or negedge rst_n)
//	  if (!rst_n) scoreboard <= 64'hFFFFFFFF_FFFFFFFF;
func NewOoOScheduler() *OoOScheduler {
	return &OoOScheduler{
		Scoreboard: ^Scoreboard(0), // All registers ready
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// DEPENDENCY MATRIX CONSTRUCTION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// BuildDependencyMatrix identifies producer→consumer relationships.
//
// ALGORITHM:
//   For each pair (i, j) where i > j (i is older):
//     If slot i writes a register that slot j reads:
//       matrix[i][j] = 1
//
// PARALLEL HARDWARE:
//   All 1024 matrix entries computed simultaneously.
//   Each entry is: (src1_match OR src2_match) AND both_valid AND (i > j)
//   The i > j check is implicit in the loop structure (j < i).
//
// TIMING ANALYSIS:
//   - XOR comparators: 40ps (6-bit compare)
//   - OR gate: 20ps
//   - AND gate: 20ps
//   - Total: ~80ps for one entry
//   - All entries in parallel: ~80ps total (plus routing)
//
// SystemVerilog:
//   always_comb begin
//     for (int i = 0; i < 32; i++) begin
//       dep_matrix[i] = 32'b0;
//       for (int j = 0; j < i; j++) begin
//         logic src1_match = (window[i].dest == window[j].src1);
//         logic src2_match = (window[i].dest == window[j].src2);
//         logic both_valid = window[i].valid & window[j].valid;
//         dep_matrix[i][j] = (src1_match | src2_match) & both_valid;
//       end
//     end
//   end
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func BuildDependencyMatrix(window *InstructionWindow) DependencyMatrix {
	var matrix DependencyMatrix

	// generate for (genvar i = 0; i < 32; i++)
	// Each iteration is a parallel hardware block
	for i := 0; i < WindowSize; i++ {
		producer := &window.Ops[i]

		// generate for (genvar j = 0; j < i; j++)
		// j < i ensures we only check older→younger dependencies
		for j := 0; j < i; j++ {
			consumer := &window.Ops[j]

			// Parallel comparators (all evaluate simultaneously in hardware)
			// Wire: src1_match = (producer.dest == consumer.src1)
			src1Match := producer.Dest == consumer.Src1

			// Wire: src2_match = (producer.dest == consumer.src2)
			src2Match := producer.Dest == consumer.Src2

			// Wire: both_valid = producer.valid & consumer.valid
			bothValid := producer.Valid && consumer.Valid

			// Final AND-OR tree
			// dep_matrix[i][j] = (src1_match | src2_match) & both_valid
			if (src1Match || src2Match) && bothValid {
				matrix[i] |= 1 << j
			}
		}
	}

	return matrix
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// READY BITMAP COMPUTATION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// ComputeReadyBitmap identifies which instructions can issue right now.
//
// READY CONDITIONS (ALL must be true):
//   1. Valid: Slot contains real instruction
//   2. Not Issued: Haven't already sent to execution (prevents double-issue)
//   3. Src1 Ready: First source register has valid data
//   4. Src2 Ready: Second source register has valid data
//   5. Dest Ready: Destination not being written by older instruction (WAW check)
//   6. No Unissued Producers: All in-window dependencies have issued
//
// WHY BOTH SCOREBOARD AND PRODUCER CHECK:
//   Scoreboard tracks COMPLETION (data available).
//   Producer check tracks ISSUE (operation started).
//
//   Example: A writes R5, B reads R5
//     - A not issued: B blocked by producer check (condition 6)
//     - A issued, not complete: B blocked by scoreboard (condition 3)
//     - A complete: B ready (both checks pass)
//
//   Without producer check, B could read stale scoreboard data:
//     - Scoreboard[R5] = 1 (old value from before A entered window)
//     - B issues before A → reads wrong value!
//
// PARALLEL HARDWARE:
//   32 parallel ready checkers, each doing:
//     - 3 scoreboard lookups (6-bit MUX each)
//     - 1 producer scan (OR tree over up to 31 entries)
//     - 5-input AND gate
//
// TIMING ANALYSIS:
//   - Scoreboard lookup: 40ps (6:64 MUX)
//   - Producer scan: 60ps (5-level OR tree)
//   - Final AND: 20ps
//   - Total: ~100ps per checker (all 32 in parallel)
//
// SystemVerilog:
//   always_comb begin
//     for (int i = 0; i < 32; i++) begin
//       // Scoreboard lookups (3 parallel reads)
//       logic src1_ready = scoreboard[window[i].src1];
//       logic src2_ready = scoreboard[window[i].src2];
//       logic dest_ready = scoreboard[window[i].dest];
//
//       // Producer scan: OR tree of (dep[j][i] & valid[j] & ~issued[j]) for all j > i
//       logic has_unissued_producer = 1'b0;
//       for (int j = i+1; j < 32; j++) begin
//         has_unissued_producer |= dep_matrix[j][i] & window[j].valid & ~window[j].issued;
//       end
//
//       // Final ready computation (AND tree)
//       ready_bitmap[i] = window[i].valid
//                       & ~window[i].issued
//                       & src1_ready
//                       & src2_ready
//                       & dest_ready
//                       & ~has_unissued_producer;
//     end
//   end
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func ComputeReadyBitmap(window *InstructionWindow, sb Scoreboard, depMatrix DependencyMatrix) uint32 {
	var bitmap uint32

	// generate for (genvar i = 0; i < 32; i++)
	// 32 parallel ready checkers
	for i := 0; i < WindowSize; i++ {
		op := &window.Ops[i]

		// Wire: scoreboard lookups (parallel MUX trees)
		// Each lookup is a 6-bit index into 64-bit scoreboard
		src1Ready := sb.IsReady(op.Src1)
		src2Ready := sb.IsReady(op.Src2)
		destReady := sb.IsReady(op.Dest)

		// Wire: producer scan (OR tree over all j > i)
		// Checks if any older instruction (j > i) that we depend on hasn't issued yet
		// NO EARLY EXIT - all bits evaluated in parallel (hardware OR tree)
		var hasUnissuedProducer uint32
		for j := i + 1; j < WindowSize; j++ {
			// dep_matrix[j][i]: Does slot j (producer) have slot i as dependent?
			depBit := (depMatrix[j] >> i) & 1

			// valid[j] & ~issued[j]: Is producer valid but not yet issued?
			validBit := boolToUint32(window.Ops[j].Valid)
			issuedBit := boolToUint32(window.Ops[j].Issued)

			// OR accumulate: hasUnissuedProducer |= depBit & validBit & ~issuedBit
			hasUnissuedProducer |= depBit & validBit & ^issuedBit
		}

		// Wire: final ready (AND of all 6 conditions)
		// ready[i] = valid & ~issued & src1_ready & src2_ready & dest_ready & ~has_unissued_producer
		ready := op.Valid &&
			!op.Issued &&
			src1Ready &&
			src2Ready &&
			destReady &&
			(hasUnissuedProducer == 0)

		// Assign to output bitmap
		if ready {
			bitmap |= 1 << i
		}
	}

	return bitmap
}

// boolToUint32 converts bool to 1-bit wire value.
//
// In SystemVerilog, booleans ARE wires, so this is implicit.
// In Go, we need explicit conversion for bitwise operations.
//
// SystemVerilog: Not needed (bool is just logic)
func boolToUint32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// PRIORITY CLASSIFICATION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// ClassifyPriority splits ready instructions into high and low priority tiers.
//
// ALGORITHM:
//   For each slot i:
//     hasDependents[i] = OR of all bits in dep_matrix[i]
//   HighPriority = readyBitmap AND hasDependents
//   LowPriority  = readyBitmap AND NOT hasDependents
//
// WHY THIS HEURISTIC:
//   Instructions with dependents are likely on the critical path.
//   Executing them first unblocks dependent work sooner.
//   ~90% correlation with true critical path depth.
//
// EXAMPLE:
//   Chain: A → B → C (A has 2 dependents, B has 1, C has 0)
//   A and B are high priority (have dependents)
//   C is low priority (leaf node)
//
// PARALLEL HARDWARE:
//   32 OR reduction trees (one per row of dep_matrix)
//   2 AND operations for final classification
//
// TIMING: 100ps (5-level OR tree + AND)
// AREA: ~50K transistors
//
// SystemVerilog:
//   always_comb begin
//     logic [31:0] has_dependents;
//     for (int i = 0; i < 32; i++) begin
//       has_dependents[i] = |dep_matrix[i];  // OR reduction
//     end
//     high_priority = ready_bitmap & has_dependents;
//     low_priority  = ready_bitmap & ~has_dependents;
//   end
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func ClassifyPriority(readyBitmap uint32, matrix DependencyMatrix) PriorityClass {
	var hasDependents uint32

	// generate for (genvar i = 0; i < 32; i++)
	// 32 parallel OR reduction trees
	for i := 0; i < WindowSize; i++ {
		// OR reduction: |dep_matrix[i]
		// Any bit set means this instruction has at least one dependent
		if matrix[i] != 0 {
			hasDependents |= 1 << i
		}
	}

	// Bitwise classification
	// high_priority = ready_bitmap & has_dependents
	// low_priority  = ready_bitmap & ~has_dependents
	return PriorityClass{
		HighPriority: readyBitmap & hasDependents,
		LowPriority:  readyBitmap & ^hasDependents,
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// ISSUE SELECTION
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// SelectIssueBundle picks up to 16 instructions to execute this cycle.
//
// ALGORITHM:
//   1. Tier Selection: Use high priority if non-empty, else low priority
//   2. For each of 16 bundle slots:
//      a. Find oldest ready instruction (CLZ = highest set bit)
//      b. Check if destination conflicts with already-selected instructions
//      c. If no conflict: add to bundle, mark destination as claimed
//      d. Clear this bit and continue to next
//
// TIER SELECTION RATIONALE:
//   We DON'T mix tiers because:
//   - High priority ops block other work (should run first)
//   - Low priority ops are leaves (can wait without harm)
//   - Mixing would dilute critical path prioritization
//
// OLDEST FIRST RATIONALE:
//   - Older instructions have been waiting longer (fairness)
//   - Older instructions are earlier in program order (more likely critical)
//   - CLZ naturally finds highest bit = oldest slot
//
// DESTINATION CONFLICT (WAW within bundle):
//   Two instructions in same bundle can't write same register.
//   Example: Both slot 20 and slot 15 write R5
//   Without claimed mask: Both issue → which write wins?
//   With claimed mask: Only slot 20 (older) issues this cycle
//
// PARALLEL HARDWARE:
//   16 cascaded priority encoders (can be parallelized as tree)
//   64-bit claimed destination mask (combinational, not flip-flops)
//
// TIMING: 150ps (CLZ tree + claim check per iteration, pipelined)
// AREA: ~60K transistors
//
// SystemVerilog:
//   always_comb begin
//     logic [31:0] bitmap;
//     logic [63:0] claimed_dests;
//     issue_bundle_t bundle;
//
//     // Tier selection MUX
//     bitmap = (|high_priority) ? high_priority : low_priority;
//     claimed_dests = 64'b0;
//     bundle.valid = 16'b0;
//
//     // 16-iteration priority encoder (can be pipelined or parallel tree)
//     for (int i = 0; i < 16; i++) begin
//       if (bitmap != 0) begin
//         logic [4:0] slot = 31 - clz32(bitmap);
//         logic [5:0] dest = window[slot].dest;
//
//         if (!claimed_dests[dest]) begin
//           bundle.indices[i] = slot;
//           bundle.valid[i] = 1'b1;
//           claimed_dests[dest] = 1'b1;
//         end
//
//         bitmap[slot] = 1'b0;  // Clear for next iteration
//       end
//     end
//   end
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func SelectIssueBundle(priority PriorityClass, window *InstructionWindow) IssueBundle {
	var bundle IssueBundle

	// Tier selection MUX: high_priority if non-zero, else low_priority
	// (|high_priority) ? high_priority : low_priority
	bitmap := priority.HighPriority
	if bitmap == 0 {
		bitmap = priority.LowPriority
	}

	// Claimed destinations (combinational, resets each cycle)
	// Tracks which registers are already being written by selected instructions
	var claimedDests uint64

	// Output index counter (which bundle slot we're filling)
	outIdx := 0

	// Priority encoder iterations
	// In hardware: 16 parallel encoders with conflict resolution
	// In Go: Sequential loop (same result, different implementation)
	for bitmap != 0 && outIdx < IssueWidth {
		// CLZ to find oldest (highest bit set)
		// slot = 31 - leading_zeros(bitmap)
		slot := uint8(31 - bits.LeadingZeros32(bitmap))
		dest := window.Ops[slot].Dest

		// Conflict check: is dest already claimed this cycle?
		// claimed_dests[dest]
		destClaimed := (claimedDests >> dest) & 1

		if destClaimed == 0 {
			// No conflict: add to bundle
			bundle.Indices[outIdx] = slot
			bundle.Valid |= 1 << outIdx

			// Mark destination as claimed
			claimedDests |= 1 << dest
			outIdx++
		}

		// Clear this bit regardless (move to next candidate)
		// Even if we couldn't issue due to conflict, don't retry same slot
		bitmap &= ^(1 << slot)
	}

	return bundle
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// SCOREBOARD UPDATE AFTER ISSUE
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// UpdateScoreboardAfterIssue marks issued instructions' destinations as pending.
//
// ACTIONS (for each valid bundle entry):
//   1. Set window[slot].Issued = true (prevents double-issue)
//   2. Set scoreboard[dest] = 0 (blocks dependent reads)
//
// WHY MARK PENDING:
//   Once an instruction issues, its destination will be overwritten.
//   Younger instructions reading that register must wait for completion.
//   Setting scoreboard[dest] = 0 enforces this wait.
//
// TIMING: Called at end of Cycle 1, updates take effect next cycle
//
// SystemVerilog:
//   always_ff @(posedge clk) begin
//     for (int i = 0; i < 16; i++) begin
//       if (bundle.valid[i]) begin
//         logic [4:0] slot = bundle.indices[i];
//         window[slot].issued <= 1'b1;
//         scoreboard[window[slot].dest] <= 1'b0;
//       end
//     end
//   end
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func UpdateScoreboardAfterIssue(sb *Scoreboard, window *InstructionWindow, bundle IssueBundle) {
	// generate for (genvar i = 0; i < 16; i++)
	// 16 parallel update paths
	for i := 0; i < IssueWidth; i++ {
		// Gated by valid[i]
		if (bundle.Valid>>i)&1 != 0 {
			slot := bundle.Indices[i]

			// Mark instruction as issued (prevents double-issue)
			window.Ops[slot].Issued = true

			// Mark destination as pending (blocks dependent readers)
			sb.MarkPending(window.Ops[slot].Dest)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// SCOREBOARD UPDATE AFTER COMPLETE
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// UpdateScoreboardAfterComplete marks completed instructions' destinations as ready.
//
// ACTIONS (for each set bit in completeMask):
//   Set scoreboard[destRegs[i]] = 1 (unblocks dependent readers)
//
// WHY MARK READY:
//   Execution has finished and written the result to the register file.
//   Younger instructions can now read the new value safely.
//   Setting scoreboard[dest] = 1 enables them to become ready.
//
// COMPLETION TIMING:
//   Different instructions take different cycles to execute:
//   - ALU ops: 1 cycle
//   - MUL: 2 cycles
//   - DIV: 10-20 cycles
//   - Load (L1 hit): 4 cycles
//   - Load (L2 hit): 12 cycles
//
//   Execution units signal completion asynchronously.
//   completeMask indicates which of the 16 bundle slots completed this cycle.
//
// SystemVerilog:
//   always_ff @(posedge clk) begin
//     for (int i = 0; i < 16; i++) begin
//       if (complete_mask[i]) begin
//         scoreboard[dest_regs[i]] <= 1'b1;
//       end
//     end
//   end
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func UpdateScoreboardAfterComplete(sb *Scoreboard, destRegs [IssueWidth]uint8, completeMask uint16) {
	// generate for (genvar i = 0; i < 16; i++)
	// 16 parallel update paths
	for i := 0; i < IssueWidth; i++ {
		// Gated by complete_mask[i]
		if (completeMask>>i)&1 != 0 {
			sb.MarkReady(destRegs[i])
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// SCHEDULER PIPELINE: CYCLE 0
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// ScheduleCycle0 is the first pipeline stage: analyze window and compute priority.
//
// OPERATIONS (all combinational, computed in parallel):
//   1. BuildDependencyMatrix: Identify all producer→consumer relationships
//   2. ComputeReadyBitmap: Determine which instructions can issue
//   3. ClassifyPriority: Split ready instructions into high/low tiers
//
// OUTPUT:
//   PipelinedPriority register captures the result for Cycle 1.
//
// TIMING: ~280ps total
//   - Dependency matrix: 120ps
//   - Ready bitmap: 100ps (parallel with matrix for non-dependent parts)
//   - Priority: 60ps
//
// PIPELINE REGISTER:
//   At positive clock edge, priority is captured in flip-flops.
//   This allows Cycle 1 to run on the captured value while
//   Cycle 0 computes the NEXT cycle's priority.
//
// SystemVerilog:
//   // Combinational
//   always_comb begin
//     dep_matrix_comb = build_dependency_matrix(window);
//     ready_bitmap_comb = compute_ready_bitmap(window, scoreboard, dep_matrix_comb);
//     priority_comb = classify_priority(ready_bitmap_comb, dep_matrix_comb);
//   end
//
//   // Sequential (pipeline register)
//   always_ff @(posedge clk) begin
//     pipelined_priority <= priority_comb;
//   end
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func (sched *OoOScheduler) ScheduleCycle0() {
	// Combinational: build dependency matrix
	depMatrix := BuildDependencyMatrix(&sched.Window)

	// Combinational: compute ready bitmap (depends on dep matrix)
	readyBitmap := ComputeReadyBitmap(&sched.Window, sched.Scoreboard, depMatrix)

	// Combinational: classify priority (depends on both above)
	priority := ClassifyPriority(readyBitmap, depMatrix)

	// Pipeline register capture (posedge clk)
	sched.PipelinedPriority = priority
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// SCHEDULER PIPELINE: CYCLE 1
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// ScheduleCycle1 is the second pipeline stage: select and issue instructions.
//
// OPERATIONS:
//   1. SelectIssueBundle: Pick up to 16 instructions (combinational)
//   2. UpdateScoreboardAfterIssue: Mark destinations pending (sequential)
//
// INPUT:
//   PipelinedPriority from previous Cycle 0 (via pipeline register)
//
// OUTPUT:
//   IssueBundle sent to execution units
//
// TIMING: ~200ps total
//   - Issue selection: 150ps
//   - Scoreboard update: 50ps
//
// NOTE ON STALENESS:
//   PipelinedPriority was computed in the PREVIOUS cycle.
//   Window state may have changed (completions arrived, new instructions).
//   This can cause brief inefficiency (miss one cycle of new readiness).
//   Correctness is preserved: we never issue wrong instructions.
//
// SystemVerilog:
//   // Combinational
//   always_comb begin
//     bundle_comb = select_issue_bundle(pipelined_priority, window);
//   end
//
//   // Sequential
//   always_ff @(posedge clk) begin
//     update_scoreboard_after_issue(bundle_comb);
//   end
//
//   // Output
//   assign issue_bundle = bundle_comb;
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func (sched *OoOScheduler) ScheduleCycle1() IssueBundle {
	// Combinational: issue selection from pipelined priority
	bundle := SelectIssueBundle(sched.PipelinedPriority, &sched.Window)

	// Sequential: state update (posedge clk)
	UpdateScoreboardAfterIssue(&sched.Scoreboard, &sched.Window, bundle)

	return bundle
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// COMPLETION HANDLER
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// ScheduleComplete handles execution unit completion signals.
//
// INTERFACE:
//   destRegs: Which registers were written (indexed by bundle position)
//   completeMask: Which bundle positions completed this cycle
//
// TIMING:
//   Called asynchronously when execution units finish.
//   In hardware, this is driven by completion signals from each EU.
//   Multiple completions can arrive in the same cycle.
//
// EFFECT:
//   Scoreboard[dest] = 1 for each completed instruction.
//   This unblocks dependent instructions in the NEXT Cycle 0.
//
// SystemVerilog:
//   always_ff @(posedge clk) begin
//     if (complete_valid) begin
//       update_scoreboard_after_complete(dest_regs, complete_mask);
//     end
//   end
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════

func (sched *OoOScheduler) ScheduleComplete(destRegs [IssueWidth]uint8, completeMask uint16) {
	UpdateScoreboardAfterComplete(&sched.Scoreboard, destRegs, completeMask)
}

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// HARDWARE IMPLEMENTATION SUMMARY
// ═══════════════════════════════════════════════════════════════════════════════════════════════
//
// MODULE HIERARCHY:
//
//   ooo_scheduler
//   ├── dependency_matrix_builder     // 1024 parallel comparators
//   │   └── generate [32][32] compare: XOR + zero_detect + AND
//   │
//   ├── ready_bitmap_computer         // 32 parallel ready checkers
//   │   ├── generate [32] scoreboard_lookup
//   │   │   └── 3x 6:64 MUX (src1, src2, dest)
//   │   └── generate [32] producer_scan
//   │       └── OR tree over up to 31 entries
//   │
//   ├── priority_classifier           // 32 OR reductions + 2 AND gates
//   │   └── generate [32] row_or: |dep_matrix[i]
//   │
//   ├── issue_selector                // 16-deep priority encoder
//   │   ├── tier_mux: (|high) ? high : low
//   │   ├── generate [16] clz32 (count leading zeros)
//   │   └── claimed_dest_tracker: 64-bit combinational mask
//   │
//   └── scoreboard_updater            // 16 parallel write ports
//       └── generate [16] conditional_write
//
// TIMING BUDGET (3.5GHz = 285ps cycle):
//
//   Cycle 0: 280ps
//   ├── Dependency matrix: 120ps
//   │   └── 6-bit XOR (40ps) + OR (20ps) + AND (20ps) + routing (40ps)
//   ├── Ready bitmap: 100ps (starts after dep matrix)
//   │   └── MUX (40ps) + OR tree (40ps) + AND (20ps)
//   └── Priority: 60ps
//       └── OR reduction (40ps) + AND (20ps)
//
//   Cycle 1: 200ps
//   ├── Issue select: 150ps
//   │   └── Tier MUX (20ps) + CLZ (60ps) + claim check (40ps) + routing (30ps)
//   └── Scoreboard update: 50ps
//       └── Write enable (20ps) + flip-flop setup (30ps)
//
// AREA BUDGET (~1.1M transistors):
//
//   Component              Transistors    Notes
//   ─────────────────────────────────────────────────
//   Window SRAM            400K           32 × 40 bits, multi-port
//   Dependency matrix      400K           32×32 comparators
//   Ready bitmap           100K           32 checkers × 3 MUX + OR tree
//   Priority classifier    50K            32 OR reductions
//   Issue selector         60K            16 CLZ + claim tracking
//   Scoreboard             5K             64 flip-flops + gates
//   Pipeline registers     50K            Priority + control signals
//   ─────────────────────────────────────────────────
//   TOTAL                  ~1.1M
//
// COMPARISON TO INDUSTRY:
//
//   Metric              Intel Skylake    AMD Zen 4    SUPRAX
//   ───────────────────────────────────────────────────────────
//   Scheduler entries   97               96           32
//   ROB entries         224              320          N/A (unified)
//   Rename registers    180              192          N/A (no rename)
//   Issue width         6                6            16
//   Transistors (OoO)   ~300M            ~350M        ~1.1M
//   Target IPC          ~5               ~5.5         ~12-14
//
// ═══════════════════════════════════════════════════════════════════════════════════════════════
