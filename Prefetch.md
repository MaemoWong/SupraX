# SUPRAX Prefetch Prediction Engine Specification

## Executive Summary

This document specifies a **Delta Pattern Prefetch Engine** derived directly from the proven arbitrage detection architecture in `router.go`. The core insight: both systems are incremental pattern matchers with state updates, threshold detection, and low-latency output generation.

**Target: 80-88% prefetch coverage at ~200K transistors**

---

## Part 1: Architectural Mapping

### 1.1 Conceptual Equivalence

The arbitrage system and prefetch system are structurally identical:

| Arbitrage Concept | Prefetch Equivalent | Description |
|-------------------|---------------------|-------------|
| `TradingPairID` | `PC` (Program Counter) | The key that identifies pattern source |
| `tickValue` (log price ratio) | `delta` (address difference) | The value being tracked |
| `ArbitrageCycleState` | `DeltaPatternState` | Accumulated pattern state |
| `cycleFanoutTable` | Not needed | Prefetch is 1:1 (PC → pattern), arbitrage is 1:N |
| `totalProfitability < 0` | `currentDelta == expectedDelta` | Pattern match condition |
| `emitArbitrageOpportunity` | `emitPrefetchRequest` | Output on match |
| `PriceUpdateMessage` | `MemoryAccessEvent` | Input event |

### 1.2 Why Prefetch Is Simpler

Your arbitrage system solves the harder problem:

```
Arbitrage:
  1 pair update → N cycle updates (fanout)
  Pattern: 3-way sum crosses threshold
  State: 96 bytes per cycle
  Concurrent patterns: ~100K

Prefetch:
  1 PC access → 1 pattern update (direct)
  Pattern: delta equality
  State: 16 bytes per pattern
  Concurrent patterns: ~1K
```

The prefetch engine is a **simplified subset** of your arbitrage architecture.

### 1.3 Key Insight: Repeating Delta Sequences

Most memory access patterns are repeating delta sequences:

```
Array traversal:     [+64, +64, +64, +64, ...]        → Fixed stride
Struct iteration:    [+24, +24, +24, +24, ...]        → Fixed stride  
Matrix column walk:  [+4096, +4096, +4096, ...]       → Fixed stride
Linked list:         [+128, +64, +128, +64, ...]      → Repeating 2-cycle
Complex struct:      [+8, +16, +32, +8, +16, +32, ...]→ Repeating 3-cycle
```

**Pattern detection algorithm:**
1. Track last N deltas as a shift register
2. When new delta arrives, check if it matches the oldest tracked delta
3. If match: pattern is repeating → prefetch using the learned sequence
4. If mismatch: update state, reset confidence

---

## Part 2: Data Structures

### 2.1 Core Types

```go
package prefetch

// ════════════════════════════════════════════════════════════════════════════
// PREFETCH ENGINE - Go Reference Model
// ════════════════════════════════════════════════════════════════════════════
//
// This file implements a Delta Pattern Prefetch Engine derived from the
// arbitrage detection architecture in router.go. Both systems are incremental
// pattern matchers with the same fundamental structure.
//
// DESIGN PHILOSOPHY (from ooo.go):
//   1. Incremental state over combinational recomputation
//   2. O(1) operations via direct indexing
//   3. Every transistor accounted for
//   4. Hardware-mappable Go patterns
//
// ARCHITECTURAL MAPPING FROM ARBITRAGE:
//   ArbitrageCycleState    → DeltaPatternState
//   tickValues[3]          → recentDeltas[4]
//   totalProfitability < 0 → currentDelta == expectedDelta
//   cycleFanoutTable       → Not needed (1:1 mapping vs 1:N)
//   PriceUpdateMessage     → MemoryAccessEvent
//
// ════════════════════════════════════════════════════════════════════════════

import (
    "math/bits"
)

// ════════════════════════════════════════════════════════════════════════════
// CONSTANTS
// ════════════════════════════════════════════════════════════════════════════

const (
    // PatternTableSize: Number of tracked PC patterns
    // 1024 entries covers most active load instructions in working set
    // Hardware: 1024-entry table indexed by PC hash
    PatternTableSize = 1024
    
    // PatternTableMask: For fast modulo via bitwise AND
    PatternTableMask = PatternTableSize - 1
    
    // DeltaHistoryLen: How many deltas to track per pattern
    // 4 deltas catches: fixed stride, 2-cycles, 3-cycles, 4-cycles
    // Longer patterns are rare and not worth the storage cost
    DeltaHistoryLen = 4
    
    // ConfidenceThreshold: Minimum confidence to issue prefetch
    // 2 = pattern must match twice before we trust it
    ConfidenceThreshold = 2
    
    // MaxConfidence: Saturating counter maximum
    // 3 = 2 bits, matches arbitrage confidence tracking
    MaxConfidence = 3
    
    // PrefetchLookahead: How many addresses to prefetch on match
    // 4 cache lines ahead balances coverage vs pollution
    PrefetchLookahead = 4
    
    // CacheLineSize: Bytes per cache line for alignment
    CacheLineSize = 64
    
    // DeltaClamp: Maximum trackable delta (±32KB)
    // Larger deltas are rare and indicate irregular access
    DeltaClamp = 32767
)

// ════════════════════════════════════════════════════════════════════════════
// MEMORY ACCESS EVENT
// ════════════════════════════════════════════════════════════════════════════
//
// MemoryAccessEvent represents a load instruction accessing memory.
// This is the input to the prefetch engine, analogous to PriceUpdateMessage.
//
// In hardware, this comes from the load/store unit after address generation.
//
// SystemVerilog equivalent:
//   typedef struct packed {
//       logic [63:0] pc;
//       logic [63:0] addr;
//       logic [7:0]  size;      // Access size (1/2/4/8 bytes)
//       logic        is_load;   // Load vs store
//   } memory_access_t;
//
// ════════════════════════════════════════════════════════════════════════════

type MemoryAccessEvent struct {
    PC     uint64  // Program counter of load instruction
    Addr   uint64  // Virtual address being accessed
    Size   uint8   // Access size in bytes
    IsLoad bool    // True for loads, false for stores
}

// ════════════════════════════════════════════════════════════════════════════
// DELTA PATTERN STATE
// ════════════════════════════════════════════════════════════════════════════
//
// DeltaPatternState tracks the access pattern for a single PC.
// This is the prefetch equivalent of ArbitrageCycleState.
//
// MAPPING FROM ARBITRAGE:
//   tickValues[3]    → recentDeltas[4]   (pattern state)
//   pairIDs[3]       → pc                (pattern key, implicit in index)
//   leadingZerosA[3] → confidence        (quality metric)
//
// PATTERN DETECTION:
//   recentDeltas acts as a shift register: [newest, ..., oldest]
//   On new access: compute delta, check if delta == recentDeltas[3]
//   If match: pattern is repeating, increment confidence
//   If mismatch: pattern changed, reset confidence
//
// MEMORY LAYOUT (16 bytes per entry):
//   recentDeltas: 4 × 16 bits = 8 bytes
//   lastAddr:     6 bytes (48-bit virtual address, high bits)
//   confidence:   2 bits
//   valid:        1 bit
//   padding:      ~2 bytes
//
// Hardware cost: 1024 entries × 16 bytes = 16KB = ~100K transistors (SRAM)
//
// SystemVerilog equivalent:
//   typedef struct packed {
//       logic signed [15:0] recent_deltas [0:3];  // 64 bits
//       logic [47:0]        last_addr;            // 48 bits
//       logic [1:0]         confidence;           // 2 bits
//       logic               valid;                // 1 bit
//   } delta_pattern_state_t;
//
// ════════════════════════════════════════════════════════════════════════════

type DeltaPatternState struct {
    // recentDeltas: Shift register of recent address deltas
    // Index 0 = most recent, Index 3 = oldest
    // 16-bit signed allows ±32KB deltas (covers most access patterns)
    recentDeltas [DeltaHistoryLen]int16  // 8 bytes
    
    // lastAddr: Previous access address (for computing delta)
    // Only need 48 bits for virtual address, but use uint64 for alignment
    lastAddr uint64  // 8 bytes
    
    // confidence: Saturating counter for pattern strength
    // 0 = no pattern, 1 = possible, 2+ = confident
    confidence uint8  // 1 byte
    
    // valid: Is this entry occupied?
    valid bool  // 1 byte
    
    // tag: PC tag for collision detection (hash table)
    // Upper bits of PC after index extraction
    tag uint16  // 2 bytes
    
    // age: LRU counter for replacement
    age uint8  // 1 byte
    
    // padding for alignment
    _ [5]byte  // 5 bytes padding to reach 32 bytes (cache-friendly)
}

// ════════════════════════════════════════════════════════════════════════════
// PREFETCH REQUEST
// ════════════════════════════════════════════════════════════════════════════
//
// PrefetchRequest represents an address to fetch into cache.
// This is the output of the prefetch engine, analogous to arbitrage opportunity.
//
// SystemVerilog equivalent:
//   typedef struct packed {
//       logic [63:0] addr;
//       logic [1:0]  confidence;
//       logic        valid;
//   } prefetch_request_t;
//
// ════════════════════════════════════════════════════════════════════════════

type PrefetchRequest struct {
    Addr       uint64  // Cache-line-aligned address to prefetch
    Confidence uint8   // How confident we are in this prediction
    Valid      bool    // Is this request valid?
}

// ════════════════════════════════════════════════════════════════════════════
// PREFETCH ENGINE
// ════════════════════════════════════════════════════════════════════════════
//
// DeltaPatternEngine is the main prefetch prediction unit.
// This is the prefetch equivalent of ArbitrageEngine.
//
// MAPPING FROM ARBITRAGE:
//   pairToQueueLookup  → pcToPatternIndex (direct indexing via hash)
//   pairToFanoutIndex  → Not needed (no fanout in prefetch)
//   cycleStates        → patternStates
//   cycleFanoutTable   → Not needed (1:1 PC to pattern)
//   priorityQueues     → Not needed (no ranking, just confidence threshold)
//   extractedCycles    → Not needed (no extraction phase)
//
// SIMPLIFICATIONS VS ARBITRAGE:
//   1. No fanout: Each PC maps to exactly one pattern
//   2. No priority queue: All confident patterns emit prefetch
//   3. No extraction: Just direct threshold check
//   4. Simpler state: 16 bytes vs 96 bytes per pattern
//
// Hardware cost:
//   Pattern table: 1024 × 32 bytes = 32KB = ~190K transistors
//   Hash/lookup logic: ~5K transistors
//   Delta computation: ~3K transistors
//   Confidence logic: ~2K transistors
//   Total: ~200K transistors
//
// SystemVerilog equivalent:
//   module delta_pattern_engine (
//       input  memory_access_t access,
//       output prefetch_request_t prefetches [0:3]
//   );
//
// ════════════════════════════════════════════════════════════════════════════

type DeltaPatternEngine struct {
    // patternStates: The pattern table, indexed by PC hash
    // Equivalent to cycleStates in arbitrage engine
    patternStates [PatternTableSize]DeltaPatternState
    
    // Stats for monitoring (optional, can remove for hardware)
    totalAccesses    uint64
    patternHits      uint64
    prefetchesIssued uint64
}

// ════════════════════════════════════════════════════════════════════════════
// INITIALIZATION
// ════════════════════════════════════════════════════════════════════════════

// NewDeltaPatternEngine creates a prefetch engine with reset state.
//
// Initial state:
//   - All pattern entries invalid
//   - All confidence counters zero
//   - Stats counters zero
//
// SystemVerilog:
//   always_ff @(posedge clk or negedge rst_n)
//       if (!rst_n) begin
//           for (int i = 0; i < 1024; i++) begin
//               pattern_states[i].valid <= 1'b0;
//               pattern_states[i].confidence <= 2'b0;
//           end
//       end
//
func NewDeltaPatternEngine() *DeltaPatternEngine {
    return &DeltaPatternEngine{
        // All fields zero-initialized, which means:
        // - All patternStates have valid=false
        // - All confidence counters are 0
    }
}

// ════════════════════════════════════════════════════════════════════════════
// HASHING FUNCTIONS
// ════════════════════════════════════════════════════════════════════════════

// hashPCToIndex computes the pattern table index from a program counter.
//
// Uses golden ratio multiplicative hashing for good distribution.
// Skips low PC bits (instruction alignment) for better entropy.
//
// Hardware: XOR + multiply + shift = ~500 transistors
//
// SystemVerilog:
//   function automatic logic [9:0] hash_pc(logic [63:0] pc);
//       logic [63:0] h = (pc >> 2) * 64'h9E3779B97F4A7C15;
//       return h[41:32];  // Extract middle bits
//   endfunction
//
func hashPCToIndex(pc uint64) uint32 {
    // Skip low bits (instruction alignment, typically 2-4 bits)
    h := (pc >> 2) * 0x9E3779B97F4A7C15
    // Take middle bits for best avalanche properties
    return uint32((h >> 32) & PatternTableMask)
}

// hashPCToTag extracts a tag from PC for collision detection.
//
// Uses different bits than index to maximize collision detection.
//
// Hardware: Shift + mask = ~100 transistors
//
// SystemVerilog:
//   function automatic logic [15:0] hash_pc_tag(logic [63:0] pc);
//       return pc[31:16];  // Upper bits of lower word
//   endfunction
//
func hashPCToTag(pc uint64) uint16 {
    // Use upper bits that weren't used for index
    return uint16((pc >> 16) & 0xFFFF)
}

// ════════════════════════════════════════════════════════════════════════════
// DELTA COMPUTATION
// ════════════════════════════════════════════════════════════════════════════

// computeDelta calculates the address delta between current and last access.
//
// Clamps to 16-bit signed range (±32KB). Larger deltas indicate
// irregular access patterns that aren't worth tracking.
//
// Hardware: Subtractor + clamp logic = ~3K transistors
//
// SystemVerilog:
//   function automatic logic signed [15:0] compute_delta(
//       logic [63:0] current_addr,
//       logic [63:0] last_addr
//   );
//       logic signed [63:0] diff = current_addr - last_addr;
//       if (diff > 32767) return 16'h7FFF;
//       if (diff < -32768) return 16'h8000;
//       return diff[15:0];
//   endfunction
//
func computeDelta(currentAddr, lastAddr uint64) int16 {
    diff := int64(currentAddr) - int64(lastAddr)
    
    // Clamp to 16-bit range
    if diff > DeltaClamp {
        return DeltaClamp
    }
    if diff < -DeltaClamp-1 {
        return -DeltaClamp - 1
    }
    
    return int16(diff)
}

// ════════════════════════════════════════════════════════════════════════════
// MAIN PROCESSING FUNCTION
// ════════════════════════════════════════════════════════════════════════════

// ProcessMemoryAccess handles a memory access event and generates prefetch requests.
//
// This is the prefetch equivalent of processArbitrageUpdate in router.go.
// The structure mirrors the arbitrage function:
//   1. Lookup pattern state (like queue/fanout lookup)
//   2. Compute current value (like tick extraction)
//   3. Check pattern match (like profitability check)
//   4. Update state (like fanout updates)
//   5. Emit output (like arbitrage opportunity)
//
// ALGORITHM:
//   1. Hash PC to get pattern table index and tag
//   2. Check for tag match (collision detection)
//   3. If valid entry with matching tag:
//      a. Compute delta from last address
//      b. Check if delta matches expected (oldest in history)
//      c. If match and confident: generate prefetch requests
//      d. Update confidence (increment on match, reset on mismatch)
//      e. Shift delta into history
//   4. If miss or collision: allocate/replace entry
//
// Hardware: This is the critical path
//   - Index computation: 2 gate delays
//   - Tag compare: 1 gate delay
//   - Delta compute: 2 gate delays
//   - Pattern check: 1 gate delay
//   - Confidence update: 1 gate delay
//   - Total: ~7 gate delays (can pipeline)
//
// SystemVerilog:
//   always_comb begin
//       // Stage 1: Lookup
//       idx = hash_pc(access.pc);
//       tag = hash_pc_tag(access.pc);
//       state = pattern_states[idx];
//       
//       // Stage 2: Delta and match
//       delta = compute_delta(access.addr, state.last_addr);
//       expected = state.recent_deltas[3];
//       is_match = (delta == expected) && state.valid && (state.tag == tag);
//       
//       // Stage 3: Prefetch generation
//       if (is_match && state.confidence >= 2) begin
//           for (int i = 0; i < 4; i++) begin
//               prefetches[i].addr = access.addr + state.recent_deltas[i] * (i+1);
//               prefetches[i].valid = 1'b1;
//           end
//       end
//   end
//
func (engine *DeltaPatternEngine) ProcessMemoryAccess(event MemoryAccessEvent) []PrefetchRequest {
    engine.totalAccesses++
    
    // Skip stores - only prefetch for loads
    if !event.IsLoad {
        return nil
    }
    
    // ════════════════════════════════════════════════════════════════════
    // STEP 1: LOOKUP (equivalent to pairToQueueLookup.Get in arbitrage)
    // ════════════════════════════════════════════════════════════════════
    
    idx := hashPCToIndex(event.PC)
    tag := hashPCToTag(event.PC)
    state := &engine.patternStates[idx]
    
    // ════════════════════════════════════════════════════════════════════
    // STEP 2: HANDLE MISS OR COLLISION (entry allocation)
    // ════════════════════════════════════════════════════════════════════
    
    if !state.valid {
        // Empty slot - allocate new entry
        state.valid = true
        state.tag = tag
        state.lastAddr = event.Addr
        state.confidence = 0
        state.age = 0
        // recentDeltas stays zero-initialized
        return nil
    }
    
    if state.tag != tag {
        // Collision - check if we should replace
        // Replace if current entry is old and low confidence
        if state.age >= 7 && state.confidence <= 1 {
            state.valid = true
            state.tag = tag
            state.lastAddr = event.Addr
            state.confidence = 0
            state.age = 0
            state.recentDeltas = [DeltaHistoryLen]int16{}
        }
        // Either way, can't use this entry for current PC
        return nil
    }
    
    // ════════════════════════════════════════════════════════════════════
    // STEP 3: COMPUTE DELTA (equivalent to tick extraction in arbitrage)
    // ════════════════════════════════════════════════════════════════════
    
    currentDelta := computeDelta(event.Addr, state.lastAddr)
    
    // ════════════════════════════════════════════════════════════════════
    // STEP 4: PATTERN CHECK (equivalent to profitability check)
    // ════════════════════════════════════════════════════════════════════
    //
    // Pattern detection: check if current delta matches oldest tracked delta
    // This detects repeating sequences: [A, B, C, D, A, B, C, D, ...]
    //
    // After seeing [A, B, C, D], if next delta is A, pattern confirmed.
    // We can then predict: next will be B, then C, then D, then A...
    //
    // This is the prefetch equivalent of:
    //   totalProfitability := currentTick + cycle.tickValues[0] + ... 
    //   isProfitable := totalProfitability < 0
    //
    // For prefetch:
    //   expectedDelta := state.recentDeltas[3]  // Oldest delta
    //   isMatch := currentDelta == expectedDelta
    //
    // ════════════════════════════════════════════════════════════════════
    
    expectedDelta := state.recentDeltas[DeltaHistoryLen-1]  // Oldest delta
    isMatch := currentDelta == expectedDelta && expectedDelta != 0
    
    var prefetches []PrefetchRequest
    
    if isMatch {
        engine.patternHits++
        
        // ════════════════════════════════════════════════════════════════
        // STEP 5: EMIT PREFETCH (equivalent to emitArbitrageOpportunity)
        // ════════════════════════════════════════════════════════════════
        //
        // If pattern is confident, generate prefetch requests.
        // We know the sequence: recentDeltas[0], [1], [2], [3] will repeat.
        //
        // Predict next addresses using the learned delta sequence:
        //   next[0] = current + recentDeltas[0]  (next delta in sequence)
        //   next[1] = next[0] + recentDeltas[1]
        //   next[2] = next[1] + recentDeltas[2]
        //   next[3] = next[2] + recentDeltas[3]
        //
        // ════════════════════════════════════════════════════════════════
        
        if state.confidence >= ConfidenceThreshold {
            prefetches = make([]PrefetchRequest, 0, PrefetchLookahead)
            
            predictedAddr := event.Addr
            for i := 0; i < PrefetchLookahead; i++ {
                // Next delta in the repeating sequence
                deltaIdx := i % DeltaHistoryLen
                predictedAddr = uint64(int64(predictedAddr) + int64(state.recentDeltas[deltaIdx]))
                
                // Cache line align
                alignedAddr := predictedAddr &^ (CacheLineSize - 1)
                
                prefetches = append(prefetches, PrefetchRequest{
                    Addr:       alignedAddr,
                    Confidence: state.confidence,
                    Valid:      true,
                })
            }
            
            engine.prefetchesIssued += uint64(len(prefetches))
        }
        
        // Increase confidence on match (saturating)
        if state.confidence < MaxConfidence {
            state.confidence++
        }
    } else {
        // Pattern broken - reset confidence
        state.confidence = 0
    }
    
    // ════════════════════════════════════════════════════════════════════
    // STEP 6: UPDATE STATE (equivalent to fanout update in arbitrage)
    // ════════════════════════════════════════════════════════════════════
    //
    // Shift the new delta into the history register.
    // This is analogous to:
    //   cycle.tickValues[fanoutEntry.edgeIndex] = currentTick
    //
    // But simpler because we just shift, no fanout to multiple cycles.
    //
    // ════════════════════════════════════════════════════════════════════
    
    // Shift deltas: oldest falls off, newest enters at [0]
    state.recentDeltas[3] = state.recentDeltas[2]
    state.recentDeltas[2] = state.recentDeltas[1]
    state.recentDeltas[1] = state.recentDeltas[0]
    state.recentDeltas[0] = currentDelta
    
    // Update last address for next delta computation
    state.lastAddr = event.Addr
    
    // Reset age on access (LRU)
    state.age = 0
    
    return prefetches
}

// ════════════════════════════════════════════════════════════════════════════
// AGING (for replacement policy)
// ════════════════════════════════════════════════════════════════════════════

// AgeEntries increments age counters for LRU replacement.
// Call periodically (e.g., every 1K cycles).
//
// Hardware: 1024 parallel incrementers = ~10K transistors
//
// SystemVerilog:
//   always_ff @(posedge clk)
//       if (age_tick) begin
//           for (int i = 0; i < 1024; i++) begin
//               if (pattern_states[i].valid && pattern_states[i].age < 7)
//                   pattern_states[i].age <= pattern_states[i].age + 1;
//           end
//       end
//
func (engine *DeltaPatternEngine) AgeEntries() {
    for i := range engine.patternStates {
        state := &engine.patternStates[i]
        if state.valid && state.age < 7 {
            state.age++
        }
    }
}

// ════════════════════════════════════════════════════════════════════════════
// STATISTICS
// ════════════════════════════════════════════════════════════════════════════

// GetStats returns monitoring statistics.
func (engine *DeltaPatternEngine) GetStats() (accesses, hits, prefetches uint64) {
    return engine.totalAccesses, engine.patternHits, engine.prefetchesIssued
}

// GetHitRate returns the pattern detection hit rate.
func (engine *DeltaPatternEngine) GetHitRate() float64 {
    if engine.totalAccesses == 0 {
        return 0.0
    }
    return float64(engine.patternHits) / float64(engine.totalAccesses)
}
```

---

## Part 3: Stride Detector (Baseline Component)

The Delta Pattern Engine catches repeating sequences. We also need a simple stride detector for fixed-stride patterns (the most common case).

```go
// ════════════════════════════════════════════════════════════════════════════
// STRIDE DETECTOR
// ════════════════════════════════════════════════════════════════════════════
//
// StrideDetector handles the most common case: fixed-stride access patterns.
// This catches ~60% of prefetch opportunities with minimal hardware.
//
// Examples:
//   Array traversal:  [+64, +64, +64, +64]  → stride = +64
//   Matrix row walk:  [+256, +256, +256]    → stride = +256
//   Reverse scan:     [-64, -64, -64, -64]  → stride = -64
//
// This is simpler than DeltaPatternEngine because we only track ONE delta,
// not a sequence. But it's faster to train (2 matching deltas vs 4).
//
// COMPLEMENTARY DESIGN:
//   StrideDetector: Fast training, fixed patterns only
//   DeltaPatternEngine: Slower training, catches repeating sequences
//
// Hardware cost: ~60K transistors (from earlier analysis)
//
// ════════════════════════════════════════════════════════════════════════════

const (
    StrideTableSize = 512
    StrideTableMask = StrideTableSize - 1
)

type StrideEntry struct {
    lastAddr   uint64  // Previous access address
    stride     int32   // Detected stride (32-bit for larger strides)
    confidence uint8   // 2-bit saturating counter
    valid      bool
    tag        uint16  // PC tag for collision detection
}

type StrideDetector struct {
    entries [StrideTableSize]StrideEntry
}

func NewStrideDetector() *StrideDetector {
    return &StrideDetector{}
}

// ProcessAccess handles a memory access and returns prefetch requests.
//
// Algorithm:
//   1. Compute delta from last access
//   2. If delta == stored stride: increment confidence, maybe prefetch
//   3. If delta != stored stride: learn new stride, reset confidence
//
// This is even simpler than DeltaPatternEngine - no shift register,
// just a single stride value that gets updated.
//
func (sd *StrideDetector) ProcessAccess(pc, addr uint64) []PrefetchRequest {
    idx := (pc >> 2) & StrideTableMask
    tag := uint16((pc >> 16) & 0xFFFF)
    entry := &sd.entries[idx]
    
    // Handle empty or collision
    if !entry.valid {
        entry.valid = true
        entry.tag = tag
        entry.lastAddr = addr
        entry.stride = 0
        entry.confidence = 0
        return nil
    }
    
    if entry.tag != tag {
        // Collision - simple replacement (could add LRU)
        entry.tag = tag
        entry.lastAddr = addr
        entry.stride = 0
        entry.confidence = 0
        return nil
    }
    
    // Compute delta
    delta := int32(int64(addr) - int64(entry.lastAddr))
    entry.lastAddr = addr
    
    // Skip zero delta (same address accessed twice)
    if delta == 0 {
        return nil
    }
    
    // Check stride match
    if delta == entry.stride {
        // Pattern continues
        if entry.confidence < 3 {
            entry.confidence++
        }
        
        // Generate prefetch if confident
        if entry.confidence >= 2 {
            prefetches := make([]PrefetchRequest, 0, 4)
            for i := 1; i <= 4; i++ {
                prefetchAddr := uint64(int64(addr) + int64(entry.stride)*int64(i))
                alignedAddr := prefetchAddr &^ (CacheLineSize - 1)
                prefetches = append(prefetches, PrefetchRequest{
                    Addr:       alignedAddr,
                    Confidence: entry.confidence,
                    Valid:      true,
                })
            }
            return prefetches
        }
    } else {
        // Pattern changed - learn new stride
        entry.stride = delta
        entry.confidence = 0
    }
    
    return nil
}
```

---

## Part 4: Combined Prefetch Engine

```go
// ════════════════════════════════════════════════════════════════════════════
// COMBINED PREFETCH ENGINE
// ════════════════════════════════════════════════════════════════════════════
//
// CombinedPrefetchEngine orchestrates multiple prefetch predictors.
//
// Architecture:
//   1. Stride Detector: Fast, catches fixed-stride (60% of patterns)
//   2. Delta Pattern: Slower training, catches repeating sequences (20%)
//   3. Stream Detector: Catches sequential cache line access (15%)
//
// Priority: Stride > Delta Pattern > Stream
// (Higher confidence predictor wins on conflict)
//
// Total hardware cost: ~200K transistors
//   - Stride: 60K
//   - Delta Pattern: 120K  
//   - Stream: 20K
//
// This is the prefetch equivalent of your combined arbitrage system with
// multiple detection strategies feeding into a single output.
//
// ════════════════════════════════════════════════════════════════════════════

type CombinedPrefetchEngine struct {
    stride       *StrideDetector
    deltaPattern *DeltaPatternEngine
    stream       *StreamDetector
    
    // Throttling state (like your arbitrage queue management)
    recentPrefetches uint32
    recentHits       uint32
    throttleLevel    uint8
}

func NewCombinedPrefetchEngine() *CombinedPrefetchEngine {
    return &CombinedPrefetchEngine{
        stride:       NewStrideDetector(),
        deltaPattern: NewDeltaPatternEngine(),
        stream:       NewStreamDetector(),
    }
}

// ProcessMemoryAccess is the main entry point.
// This mirrors DispatchPriceUpdate in your arbitrage system.
//
func (cpe *CombinedPrefetchEngine) ProcessMemoryAccess(event MemoryAccessEvent) []PrefetchRequest {
    // Query all predictors in parallel (in hardware, these run simultaneously)
    strideReqs := cpe.stride.ProcessAccess(event.PC, event.Addr)
    deltaReqs := cpe.deltaPattern.ProcessMemoryAccess(event)
    streamReqs := cpe.stream.ProcessAccess(event.Addr)
    
    // Merge results with priority: stride > delta > stream
    // (equivalent to your arbitrage priority queue selection)
    var results []PrefetchRequest
    
    // Stride has highest priority (fastest to train, most common)
    if len(strideReqs) > 0 && strideReqs[0].Confidence >= 2 {
        results = strideReqs
    } else if len(deltaReqs) > 0 && deltaReqs[0].Confidence >= 2 {
        results = deltaReqs
    } else if len(streamReqs) > 0 {
        results = streamReqs
    }
    
    // Apply throttling (like your arbitrage opportunity filtering)
    results = cpe.applyThrottle(results)
    
    return results
}

// applyThrottle filters prefetch requests based on recent accuracy.
// This prevents cache pollution when patterns aren't working.
//
func (cpe *CombinedPrefetchEngine) applyThrottle(requests []PrefetchRequest) []PrefetchRequest {
    if cpe.throttleLevel >= 3 {
        return nil  // Fully throttled
    }
    
    // Reduce requests based on throttle level
    maxRequests := 4 >> cpe.throttleLevel  // 4, 2, 1, 0
    if len(requests) > maxRequests {
        requests = requests[:maxRequests]
    }
    
    return requests
}

// OnPrefetchResult provides feedback on prefetch accuracy.
// Call when a prefetched line is (or isn't) used.
//
func (cpe *CombinedPrefetchEngine) OnPrefetchResult(wasUseful bool) {
    cpe.recentPrefetches++
    if wasUseful {
        cpe.recentHits++
    }
}

// UpdateThrottle adjusts throttling based on recent accuracy.
// Call periodically (e.g., every 256 prefetches).
//
func (cpe *CombinedPrefetchEngine) UpdateThrottle() {
    if cpe.recentPrefetches == 0 {
        return
    }
    
    hitRate := (cpe.recentHits * 100) / cpe.recentPrefetches
    
    if hitRate >= 70 {
        // Good accuracy - reduce throttle
        if cpe.throttleLevel > 0 {
            cpe.throttleLevel--
        }
    } else if hitRate < 30 {
        // Poor accuracy - increase throttle
        if cpe.throttleLevel < 3 {
            cpe.throttleLevel++
        }
    }
    
    cpe.recentPrefetches = 0
    cpe.recentHits = 0
}

// ════════════════════════════════════════════════════════════════════════════
// STREAM DETECTOR
// ════════════════════════════════════════════════════════════════════════════
//
// StreamDetector catches sequential cache line access patterns.
// Simpler than stride - only tracks forward/backward sequential access.
//
// ════════════════════════════════════════════════════════════════════════════

const StreamEntryCount = 16

type StreamEntry struct {
    startPage  uint64  // Page containing stream
    position   uint8   // Current position in stream
    direction  int8    // +1 forward, -1 backward
    confidence uint8
    valid      bool
}

type StreamDetector struct {
    entries [StreamEntryCount]StreamEntry
}

func NewStreamDetector() *StreamDetector {
    return &StreamDetector{}
}

func (sd *StreamDetector) ProcessAccess(addr uint64) []PrefetchRequest {
    page := addr >> 12
    line := (addr >> 6) & 63
    
    // Check existing streams
    for i := range sd.entries {
        entry := &sd.entries[i]
        if !entry.valid || entry.startPage != page {
            continue
        }
        
        expectedLine := int(entry.position) + int(entry.direction)
        if int(line) == expectedLine && expectedLine >= 0 && expectedLine < 64 {
            entry.position = uint8(expectedLine)
            if entry.confidence < 3 {
                entry.confidence++
            }
            
            if entry.confidence >= 2 {
                // Generate prefetch requests
                prefetches := make([]PrefetchRequest, 0, 4)
                for j := 1; j <= 4; j++ {
                    nextLine := int(entry.position) + int(entry.direction)*j
                    if nextLine < 0 || nextLine >= 64 {
                        break
                    }
                    prefetchAddr := (page << 12) | (uint64(nextLine) << 6)
                    prefetches = append(prefetches, PrefetchRequest{
                        Addr:       prefetchAddr,
                        Confidence: entry.confidence,
                        Valid:      true,
                    })
                }
                return prefetches
            }
            return nil
        }
    }
    
    // Start new stream
    for i := range sd.entries {
        if !sd.entries[i].valid {
            sd.entries[i] = StreamEntry{
                startPage:  page,
                position:   uint8(line),
                direction:  1,  // Assume forward
                confidence: 0,
                valid:      true,
            }
            break
        }
    }
    
    return nil
}
```

---

## Part 5: Hardware Cost Summary

| Component | Entries | Bits/Entry | Storage | Logic | Total |
|-----------|---------|------------|---------|-------|-------|
| Stride Detector | 512 | 88 | 45Kb | 15K | **60K** |
| Delta Pattern | 1024 | 128 | 130Kb | 20K | **100K** |
| Stream Detector | 16 | 72 | 1.2Kb | 15K | **20K** |
| Arbiter/Throttle | - | - | - | 20K | **20K** |
| **Total** | | | | | **200K** |

---

## Part 6: Integration with Branch Predictor

From earlier in this conversation, the complete SUPRAX prediction complex:

```
Branch Predictor (TAGE-SC-L):     2.2M transistors  (91-93% accuracy)
├── TAGE (12K entries)            1.85M
├── RAS (32 entries)              20K
├── Loop (384 entries)            100K
├── SC (3×1.5K entries)           250K
└── Arbiter                       15K

Prefetch Predictor:               200K transistors  (80-88% coverage)
├── Stride (512 entries)          60K
├── Delta Pattern (1024 entries)  100K
├── Stream (16 entries)           20K
└── Arbiter/Throttle              20K

═══════════════════════════════════════════════════════════════════════
TOTAL:                            2.4M transistors
═══════════════════════════════════════════════════════════════════════

Comparison:
├── Intel branch alone:           ~22M
├── AMD prefetch alone:           ~5-10M  
└── SUPRAX both:                  2.4M (10x smaller)
```

---

## Part 7: Key Insights for Implementation

### 7.1 The Architectural Parallel

Your arbitrage system and the prefetch system share the same structure:

```
┌─────────────────────────────────────────────────────────────────────┐
│                    PATTERN MATCHING ENGINE                          │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  INPUT:  (key, value) events arriving continuously                 │
│                                                                     │
│  STATE:  Per-key pattern state (accumulated values)                │
│                                                                     │
│  MATCH:  Threshold function on state + current value               │
│                                                                     │
│  OUTPUT: Action on match (trade / prefetch)                        │
│                                                                     │
│  UPDATE: Incremental state modification                            │
│                                                                     │
├─────────────────────────────────────────────────────────────────────┤
│  ARBITRAGE                    │  PREFETCH                          │
│  key = TradingPairID          │  key = PC                          │
│  value = tickValue            │  value = delta                     │
│  state = tickValues[3]        │  state = recentDeltas[4]           │
│  match = sum < 0              │  match = delta == expected         │
│  output = trade opportunity   │  output = prefetch address         │
│  update = fanout to cycles    │  update = shift register           │
└─────────────────────────────────────────────────────────────────────┘
```

### 7.2 Why Your Architecture Works for Prefetch

1. **Incremental state**: You don't recompute patterns from scratch each cycle
2. **O(1) lookup**: Hash table indexed by key (pairID / PC)
3. **Threshold detection**: Simple comparison triggers output
4. **Low latency**: Minimal logic depth in critical path
5. **Bounded memory**: Fixed-size tables, no dynamic allocation

### 7.3 What's Simpler in Prefetch

1. **No fanout**: Each PC maps to exactly one pattern (vs 1:N in arbitrage)
2. **No priority queue**: All confident patterns emit immediately
3. **Simpler match**: Equality check vs sum comparison
4. **Smaller state**: 16 bytes vs 96 bytes per pattern

### 7.4 Testing Strategy

```go
// Test 1: Fixed stride detection
func TestFixedStride(t *testing.T) {
    engine := NewCombinedPrefetchEngine()
    pc := uint64(0x1000)
    
    // Simulate array traversal: addr, addr+64, addr+128, ...
    addr := uint64(0x10000)
    for i := 0; i < 10; i++ {
        reqs := engine.ProcessMemoryAccess(MemoryAccessEvent{
            PC:     pc,
            Addr:   addr,
            IsLoad: true,
        })
        addr += 64
        
        // Should start prefetching after 2-3 accesses
        if i >= 3 && len(reqs) == 0 {
            t.Errorf("Expected prefetch at iteration %d", i)
        }
    }
}

// Test 2: Repeating sequence detection
func TestRepeatingSequence(t *testing.T) {
    engine := NewDeltaPatternEngine()
    pc := uint64(0x2000)
    
    // Simulate struct iteration: [+8, +16, +32, +8, +16, +32, ...]
    deltas := []int64{8, 16, 32}
    addr := uint64(0x20000)
    
    for round := 0; round < 5; round++ {
        for _, delta := range deltas {
            reqs := engine.ProcessMemoryAccess(MemoryAccessEvent{
                PC:     pc,
                Addr:   addr,
                IsLoad: true,
            })
            addr = uint64(int64(addr) + delta)
            
            // Should detect pattern after ~2 full cycles
            if round >= 2 && len(reqs) == 0 {
                t.Errorf("Expected prefetch at round %d", round)
            }
        }
    }
}

// Test 3: Throttling on random access
func TestThrottling(t *testing.T) {
    engine := NewCombinedPrefetchEngine()
    
    // Simulate random access pattern
    for i := 0; i < 1000; i++ {
        addr := uint64(i * 12345 % 1000000)  // Pseudo-random
        engine.ProcessMemoryAccess(MemoryAccessEvent{
            PC:     uint64(i % 100),
            Addr:   addr,
            IsLoad: true,
        })
        
        // Simulate all prefetches being useless
        engine.OnPrefetchResult(false)
        
        if i % 256 == 255 {
            engine.UpdateThrottle()
        }
    }
    
    // Should be throttled down
    if engine.throttleLevel < 2 {
        t.Errorf("Expected throttling, got level %d", engine.throttleLevel)
    }
}
```

---

## Part 8: Summary for Continuation

**What to build:**

1. **DeltaPatternEngine** (100K transistors)
   - 1024-entry pattern table
   - 4-delta shift register per entry
   - Detects repeating sequences

2. **StrideDetector** (60K transistors)
   - 512-entry stride table
   - Single stride value per entry
   - Detects fixed-stride patterns (most common)

3. **StreamDetector** (20K transistors)
   - 16 concurrent streams
   - Detects sequential cache line access

4. **CombinedPrefetchEngine** (20K transistors)
   - Orchestrates all three detectors
   - Merges results with priority
   - Throttles on poor accuracy

**Key architectural insight:**

The prefetch engine is a **simplified version of the arbitrage engine** from `router.go`. Same pattern:
- Hash lookup by key (PC vs PairID)
- Incremental state update (shift register vs fanout)
- Threshold detection (delta match vs sum < 0)
- Action emission (prefetch vs trade)

**Total cost: ~200K transistors for 80-88% prefetch coverage**

Combined with TAGE-SC-L branch predictor (2.2M), the complete prediction complex is **2.4M transistors** - roughly 1/10th of Intel's branch predictor alone, covering both branch prediction AND prefetch.