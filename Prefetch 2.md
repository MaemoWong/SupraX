# SUPRAX Prediction Infrastructure - Supporting Components

## Part 9: Supporting Infrastructure from Arbitrage System

The arbitrage detection system includes several high-performance components that can be directly reused or adapted for the prefetch engine. This section documents these components for the next Claude to understand.

---

## 9.1 PooledQuantumQueue - Hierarchical Bitmap Priority Queue

### 9.1.1 Overview

The `PooledQuantumQueue` is a zero-allocation priority queue using three-level bitmap indexing. It provides O(1) minimum finding using hardware CLZ (Count Leading Zeros) instructions.

**Why this matters for prefetch:** While the basic prefetch engine uses confidence thresholds, a priority queue could rank prefetch candidates by confidence or predicted utility.

### 9.1.2 Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                    THREE-LEVEL BITMAP HIERARCHY                     │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Level 0: Global Summary (1 × 64 bits)                             │
│  ┌────────────────────────────────────────────────────────────┐    │
│  │ Bit 0 │ Bit 1 │ Bit 2 │ ... │ Bit 63 │                    │    │
│  │   ↓   │   ↓   │   ↓   │     │   ↓    │                    │    │
│  └───┼───┴───┼───┴───┼───┴─────┴───┼────┘                    │    │
│      │       │       │             │                          │    │
│      ▼       ▼       ▼             ▼                          │    │
│  Level 1: Group Summaries (64 × 64 bits)                          │
│  ┌──────┐ ┌──────┐ ┌──────┐     ┌──────┐                         │
│  │Grp 0 │ │Grp 1 │ │Grp 2 │ ... │Grp 63│                         │
│  │64 bit│ │64 bit│ │64 bit│     │64 bit│                         │
│  └──┬───┘ └──────┘ └──────┘     └──────┘                         │
│     │                                                             │
│     ▼                                                             │
│  Level 2: Lane Masks (64 groups × 64 lanes × 64 bits each)       │
│  ┌──────┐ ┌──────┐ ┌──────┐     ┌──────┐                         │
│  │Lane 0│ │Lane 1│ │Lane 2│ ... │Lane63│                         │
│  │64 bit│ │64 bit│ │64 bit│     │64 bit│                         │
│  └──┬───┘ └──────┘ └──────┘     └──────┘                         │
│     │                                                             │
│     ▼                                                             │
│  Buckets: 262,144 priority levels (doubly-linked lists)          │
│  ┌─────┐ ┌─────┐ ┌─────┐       ┌─────┐                           │
│  │Bkt 0│→│Bkt 1│→│Bkt 2│→ ... →│Bkt N│                           │
│  └─────┘ └─────┘ └─────┘       └─────┘                           │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 9.1.3 Key Operations

**Finding Minimum (O(1) with CLZ):**

```go
func (q *PooledQuantumQueue) PeepMin() (Handle, int64, uint64) {
    // Three CLZ operations to find minimum priority bucket
    g := bits.LeadingZeros64(q.summary)        // Find first active group
    gb := &q.groups[g]
    l := bits.LeadingZeros64(gb.l1Summary)     // Find first active lane in group
    t := bits.LeadingZeros64(gb.l2[l])         // Find first active bucket in lane

    // Reconstruct bucket index from hierarchical components
    // Group (6 bits) | Lane (6 bits) | Bucket (6 bits) = 18 bits = 262,144 buckets
    b := Handle((uint64(g) << 12) | (uint64(l) << 6) | uint64(t))
    h := q.buckets[b]

    entry := q.entry(h)
    return h, entry.Tick, entry.Data
}
```

**Priority Update (O(1) amortized):**

```go
func (q *PooledQuantumQueue) MoveTick(h Handle, newTick int64) {
    entry := q.entry(h)

    // No-op if priority unchanged (common case optimization)
    if entry.Tick == newTick {
        return
    }

    // Unlink from old bucket, link to new bucket
    q.unlink(h)
    q.linkAtHead(h, newTick)
}
```

### 9.1.4 Memory Layout

```go
// Entry: 32 bytes, cache-aligned
type Entry struct {
    Tick int64  // 8B - Priority value (-1 when free)
    Data uint64 // 8B - User payload
    Next Handle // 8B - Next in doubly-linked list
    Prev Handle // 8B - Previous in doubly-linked list
}

// Queue: Hot data in first cache line
type PooledQuantumQueue struct {
    // Cache line 1: Hot metadata (24B used, 40B padding)
    summary uint64  // 8B - Global active groups mask
    size    int     // 8B - Current entry count
    arena   uintptr // 8B - Base pointer to shared pool
    _       [40]byte

    // Large arrays (cache-aligned, accessed based on operation)
    buckets [262144]Handle   // Per-tick chain heads
    groups  [64]groupBlock   // Hierarchical summaries
}
```

### 9.1.5 Shared Memory Pool

Multiple queues share a single memory pool for cache efficiency:

```go
// Shared arena initialization
engine.sharedArena = make([]pooledquantumqueue.Entry, totalCycles)
arenaPtr := unsafe.Pointer(&engine.sharedArena[0])

// Multiple queues use same arena
for i := range engine.priorityQueues {
    newQueue := pooledquantumqueue.New(arenaPtr)
    engine.priorityQueues[i] = *newQueue
}

// Handle-based addressing into shared pool
func (q *PooledQuantumQueue) entry(h Handle) *Entry {
    // Shift by 5 for 32-byte entries (2^5 = 32)
    return (*Entry)(unsafe.Pointer(q.arena + uintptr(h)<<5))
}
```

### 9.1.6 Application to Prefetch

For the prefetch engine, you might use PooledQuantumQueue to:

1. **Rank prefetch candidates by confidence:** Higher confidence prefetches get priority
2. **Manage prefetch queue depth:** Only issue top-N most confident prefetches per cycle
3. **Age out stale predictions:** Use tick values as timestamps, evict old entries

```go
// Hypothetical prefetch queue usage
type PrefetchQueueEntry struct {
    Addr       uint64  // Prefetch address
    Confidence uint8   // Prediction confidence
    PC         uint64  // Source PC for debugging
}

// Priority = (MaxConfidence - confidence) to make high confidence = low tick = first out
func confidenceToPriority(confidence uint8) int64 {
    return int64(MaxConfidence - confidence)
}
```

---

## 9.2 Ring56 - Lock-Free SPSC Ring Buffer

### 9.2.1 Overview

`Ring56` is a single-producer single-consumer (SPSC) lock-free ring buffer for inter-core communication. It passes 56-byte messages with wait-free guarantees.

**Why this matters for prefetch:** In a multi-core SUPRAX implementation, prefetch requests might be generated on one core and consumed by the memory subsystem on another.

### 9.2.2 Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                      SPSC RING BUFFER                               │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Producer (Core A)                    Consumer (Core B)             │
│  ┌──────────────┐                    ┌──────────────┐              │
│  │   tail ptr   │                    │   head ptr   │              │
│  │  (write pos) │                    │  (read pos)  │              │
│  └──────┬───────┘                    └──────┬───────┘              │
│         │                                   │                       │
│         ▼                                   ▼                       │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │ Slot 0 │ Slot 1 │ Slot 2 │ ... │ Slot N-1 │                 │   │
│  │ 64B    │ 64B    │ 64B    │     │ 64B      │                 │   │
│  │┌─────┐│┌─────┐│┌─────┐│     │┌─────┐│                 │   │
│  ││val  │││val  │││val  ││     ││val  ││                 │   │
│  ││56B  │││56B  │││56B  ││     ││56B  ││                 │   │
│  │├─────┤│├─────┤│├─────┤│     │├─────┤│                 │   │
│  ││seq  │││seq  │││seq  ││     ││seq  ││                 │   │
│  ││8B   │││8B   │││8B   ││     ││8B   ││                 │   │
│  │└─────┘│└─────┘│└─────┘│     │└─────┘│                 │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  Sequence Protocol:                                                 │
│  - seq == tail: Slot is FREE (producer can write)                  │
│  - seq == tail+1: Slot has DATA (consumer can read)                │
│  - seq == head+step: Slot is RELEASED (back to free pool)          │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 9.2.3 Key Operations

**Push (Producer - Wait-Free):**

```go
func (r *Ring) Push(val *[56]byte) bool {
    t := r.tail
    s := &r.buf[t&r.mask]

    // Check if slot is available (sequence == tail means free)
    if atomic.LoadUint64(&s.seq) != t {
        return false // Buffer full
    }

    // Copy payload
    s.val = *val

    // Signal data availability (sequence = tail + 1)
    atomic.StoreUint64(&s.seq, t+1)

    // Advance producer position
    r.tail = t + 1
    return true
}
```

**Pop (Consumer - Wait-Free):**

```go
func (r *Ring) Pop() *[56]byte {
    h := r.head
    s := &r.buf[h&r.mask]

    // Check if data is available (sequence == head + 1 means data ready)
    if atomic.LoadUint64(&s.seq) != h+1 {
        return nil // Buffer empty
    }

    // Extract payload pointer
    val := &s.val

    // Mark slot as free (sequence = head + step)
    atomic.StoreUint64(&s.seq, h+r.step)

    // Advance consumer position
    r.head = h + 1
    return val
}
```

### 9.2.4 Cache Line Isolation

```go
type Ring struct {
    _    [64]byte // Cache line 1: Isolation padding
    head uint64   // Cache line 2: Consumer position (only consumer writes)

    _    [56]byte // Padding to next cache line
    tail uint64   // Cache line 4: Producer position (only producer writes)

    _ [56]byte    // Reserved space

    // Configuration (read-only after init)
    mask uint64   // Size - 1 for efficient modulo
    step uint64   // Size for sequence wrapping
    buf  []slot   // Backing buffer

    _ [24]byte    // Tail padding
}
```

**Why this layout:**
- `head` and `tail` on separate cache lines prevents false sharing
- Producer only writes `tail`, consumer only writes `head`
- No cache line ping-pong between cores

### 9.2.5 Application to Prefetch

For multi-core prefetch:

```go
// Prefetch request message (fits in 56 bytes)
type PrefetchMessage struct {
    Addresses [4]uint64  // 32B - Up to 4 prefetch addresses
    PC        uint64     // 8B - Source PC
    Confidence uint8     // 1B - Prediction confidence
    Count     uint8      // 1B - How many addresses are valid
    _         [14]byte   // 14B - Padding to 56 bytes
}

// Core A: Generate prefetch requests
func generatePrefetch(ring *ring56.Ring, addrs []uint64, pc uint64, conf uint8) {
    var msg PrefetchMessage
    msg.PC = pc
    msg.Confidence = conf
    msg.Count = uint8(len(addrs))
    for i, addr := range addrs {
        if i < 4 {
            msg.Addresses[i] = addr
        }
    }
    
    msgBytes := (*[56]byte)(unsafe.Pointer(&msg))
    ring.Push(msgBytes)
}

// Memory subsystem: Consume prefetch requests
func consumePrefetch(ring *ring56.Ring) {
    for {
        if p := ring.Pop(); p != nil {
            msg := (*PrefetchMessage)(unsafe.Pointer(p))
            for i := uint8(0); i < msg.Count; i++ {
                issuePrefetchToL2(msg.Addresses[i])
            }
        }
    }
}
```

---

## 9.3 FastUni - Logarithmic Computation

### 9.3.1 Overview

`fastuni` provides high-performance logarithm computation using bit manipulation and polynomial approximation. Used for price ratio calculations in arbitrage.

**Why this matters for prefetch:** Similar math might be useful for confidence calculations or adaptive throttling based on hit rates.

### 9.3.2 Key Algorithm: log2 via Bit Manipulation

```go
func log2u64(x uint64) float64 {
    // Find position of most significant bit (integer part of log2)
    k := 63 - bits.LeadingZeros64(x)
    lead := uint64(1) << k

    // Extract fractional bits
    frac := x ^ lead

    // Normalize to IEEE 754 mantissa position
    if k > 52 {
        frac >>= uint(k - 52)
    } else {
        frac <<= uint(52 - k)
    }

    // Reconstruct normalized double in range [1, 2)
    mBits := (uint64(1023) << 52) | (frac & fracMask)
    m := math.Float64frombits(mBits)

    // Integer part + fractional part via polynomial
    return float64(k) + ln1pf(m-1)*invLn2
}
```

### 9.3.3 Polynomial Approximation

```go
// ln(1+f) ≈ f*(c1 + f*(c2 + f*(c3 + f*(c4 + f*c5))))
// Horner's method minimizes multiplications
func ln1pf(f float64) float64 {
    t := f*c5 + c4
    t = f*t + c3
    t = f*t + c2
    t = f*t + c1
    return f * t
}

// Coefficients for 5th-order approximation
const (
    c1 = +0.9990102443771056
    c2 = -0.4891559897950173
    c3 = +0.2833026021012029
    c4 = -0.1301181019014788
    c5 = +0.0301022874045224
)
```

### 9.3.4 Application to Prefetch

For adaptive prefetch throttling based on hit rate:

```go
// Logarithmic decay for throttle adjustment
func calculateThrottleAdjustment(hitRate float64) float64 {
    // Use log to create smooth adjustment curve
    // High hit rate (0.8+) -> negative adjustment (less throttle)
    // Low hit rate (0.3-) -> positive adjustment (more throttle)
    
    if hitRate <= 0 {
        return 1.0 // Maximum throttle increase
    }
    if hitRate >= 1 {
        return -1.0 // Maximum throttle decrease
    }
    
    // log2(hitRate/0.5) gives smooth curve centered at 50% hit rate
    // hitRate=0.5 -> 0, hitRate=1.0 -> 1, hitRate=0.25 -> -1
    return log2u64(uint64(hitRate * 1000)) - log2u64(500)
}
```

---

## 9.4 Utils - Memory Operations and Hashing

### 9.4.1 Overview

`utils` provides zero-allocation utilities for memory operations, hex parsing, and hashing. Critical for hot path performance.

### 9.4.2 Memory Load Operations

```go
// Direct 64-bit load - compiles to single MOV instruction
func Load64(b []byte) uint64 {
    return *(*uint64)(unsafe.Pointer(&b[0]))
}

// 128-bit load for SIMD operations
func Load128(b []byte) (uint64, uint64) {
    p := (*[2]uint64)(unsafe.Pointer(&b[0]))
    return p[0], p[1]
}
```

### 9.4.3 SIMD-Style Hex Parsing

```go
func ParseHexU64(b []byte) uint64 {
    // Process 8 hex chars as single 64-bit value
    chunk := Load64(b[:8])

    // Parallel ASCII to nibble conversion
    chunk |= 0x2020202020202020                            // Force lowercase
    letterMask := (chunk & 0x4040404040404040) >> 6        // Detect a-f
    chunk = chunk - 0x3030303030303030 - (letterMask * 39) // Convert to nibbles

    // SIMD-style nibble compaction
    extracted := chunk & 0x000F000F000F000F
    chunk ^= extracted
    chunk |= extracted << 12

    extracted = chunk & 0xFF000000FF000000
    chunk ^= extracted
    chunk |= extracted >> 24

    extracted = chunk & 0x000000000000FFFF
    chunk ^= extracted
    chunk |= extracted << 48

    return chunk >> 32
}
```

### 9.4.4 Mix64 Hash Finalization

```go
// Murmur3-style 64-bit hash finalization
// Ensures full avalanche: each input bit affects all output bits
func Mix64(x uint64) uint64 {
    x ^= x >> 33
    x *= 0xff51afd7ed558ccd
    x ^= x >> 33
    x *= 0xc4ceb9fe1a85ec53
    x ^= x >> 33
    return x
}
```

### 9.4.5 Application to Prefetch

For PC hashing in the prefetch engine:

```go
// Use Mix64 for PC-to-index hashing
func hashPCToIndex(pc uint64) uint32 {
    // Skip low bits (instruction alignment)
    h := Mix64(pc >> 2)
    return uint32(h & PatternTableMask)
}

// For prefetch address deduplication
func deduplicatePrefetches(addrs []uint64) []uint64 {
    seen := uint64(0) // Bloom filter approximation
    result := make([]uint64, 0, len(addrs))
    
    for _, addr := range addrs {
        hash := Mix64(addr)
        bit := uint64(1) << (hash & 63)
        
        if seen & bit == 0 {
            seen |= bit
            result = append(result, addr)
        }
    }
    
    return result
}
```

---

## 9.5 CountHexLeadingZeros - Liquidity Detection

### 9.5.1 Overview

This function counts leading zero characters in hex data using SIMD-style parallel processing. Used for instant liquidity approximation in arbitrage.

**Why this matters for prefetch:** Similar techniques could detect address patterns or identify sparse access regions.

### 9.5.2 Algorithm

```go
func CountHexLeadingZeros(segment []byte) int {
    const ZERO_PATTERN = 0x3030303030303030  // "00000000" in ASCII

    // Process 32 bytes in four 8-byte chunks
    c0 := Load64(segment[0:8]) ^ ZERO_PATTERN
    c1 := Load64(segment[8:16]) ^ ZERO_PATTERN
    c2 := Load64(segment[16:24]) ^ ZERO_PATTERN
    c3 := Load64(segment[24:32]) ^ ZERO_PATTERN

    // Create bitmask: bit N set if chunk N contains non-zero
    // (x|(^x+1))>>63 produces 1 if any byte non-zero, 0 if all zero
    mask := ((c0|(^c0+1))>>63)<<0 | 
            ((c1|(^c1+1))>>63)<<1 |
            ((c2|(^c2+1))>>63)<<2 | 
            ((c3|(^c3+1))>>63)<<3

    // Find first non-zero chunk
    firstChunk := bits.TrailingZeros64(mask)

    if firstChunk == 64 {
        return 32  // All zeros
    }

    // Find first non-zero byte within chunk
    chunks := [4]uint64{c0, c1, c2, c3}
    firstByte := bits.TrailingZeros64(chunks[firstChunk]) >> 3

    return (firstChunk << 3) + firstByte
}
```

### 9.5.3 The Key Trick

```go
// Check if ANY byte in a 64-bit word is non-zero
// (x | (^x + 1)) >> 63

// If x = 0x0000000000000000:
//   ^x = 0xFFFFFFFFFFFFFFFF
//   ^x + 1 = 0x0000000000000000 (overflow)
//   x | 0 = 0
//   0 >> 63 = 0  ← All zeros

// If x = 0x0000000000000001:
//   ^x = 0xFFFFFFFFFFFFFFFE
//   ^x + 1 = 0xFFFFFFFFFFFFFFFF
//   x | 0xFFFFFFFFFFFFFFFF = 0xFFFFFFFFFFFFFFFF
//   0xFFFFFFFFFFFFFFFF >> 63 = 1  ← Has non-zero
```

---

## 9.6 Integration Summary

### 9.6.1 Component Reuse Matrix

| Component | Arbitrage Use | Prefetch Use | Adaptation Needed |
|-----------|---------------|--------------|-------------------|
| PooledQuantumQueue | Rank cycles by profitability | Rank prefetches by confidence | Entry format change |
| Ring56 | Inter-core price updates | Inter-core prefetch requests | Message format change |
| Mix64 | Pair ID hashing | PC hashing | Direct reuse |
| Load64/Load128 | Reserve parsing | Pattern state access | Direct reuse |
| ParseHexU64 | Address parsing | Not needed | N/A |
| CountHexLeadingZeros | Liquidity detection | Pattern analysis (optional) | Adapt for addresses |

### 9.6.2 Common Patterns

Both systems share these architectural patterns:

1. **Hash-indexed lookup tables** with collision handling
2. **Incremental state updates** (not recomputation)
3. **Threshold-based detection** (profitability < 0, confidence >= 2)
4. **Bitmap-based tracking** for O(1) operations
5. **Cache-line-aware layout** to prevent false sharing
6. **Zero-allocation hot paths** using pre-allocated pools

### 9.6.3 Key Differences

| Aspect | Arbitrage | Prefetch |
|--------|-----------|----------|
| Fanout | 1:N (pair → cycles) | 1:1 (PC → pattern) |
| Pattern complexity | 3-way sum | 4-element equality |
| State size | 96B per cycle | 32B per pattern |
| Output | Trade opportunity | Prefetch address |
| Priority queue | Essential (rank by profit) | Optional (confidence threshold) |
| Inter-core comm | Essential (price distribution) | Optional (depends on arch) |

---

## Part 10: Complete Implementation Checklist

For the next Claude, here's what needs to be built:

### 10.1 Core Prefetch Engine

- [ ] `DeltaPatternState` struct (32 bytes, cache-aligned)
- [ ] `DeltaPatternEngine` with 1024-entry table
- [ ] `hashPCToIndex()` using Mix64
- [ ] `hashPCToTag()` for collision detection
- [ ] `computeDelta()` with clamping
- [ ] `ProcessMemoryAccess()` main loop
- [ ] `AgeEntries()` for LRU replacement

### 10.2 Stride Detector

- [ ] `StrideEntry` struct
- [ ] `StrideDetector` with 512-entry table
- [ ] `ProcessAccess()` for stride detection

### 10.3 Stream Detector

- [ ] `StreamEntry` struct
- [ ] `StreamDetector` with 16 concurrent streams
- [ ] `ProcessAccess()` for stream detection

### 10.4 Combined Engine

- [ ] `CombinedPrefetchEngine` orchestrator
- [ ] Priority-based result merging
- [ ] Throttling state machine
- [ ] `OnPrefetchResult()` feedback loop
- [ ] `UpdateThrottle()` periodic adjustment

### 10.5 Testing

- [ ] Fixed stride pattern test
- [ ] Repeating sequence test
- [ ] Stream detection test
- [ ] Throttling under random access test
- [ ] Cache pollution prevention test

### 10.6 Hardware Mapping

- [ ] SystemVerilog module interfaces
- [ ] Pipeline stage definitions
- [ ] Timing constraints
- [ ] Transistor budget verification

---

## Part 11: Final Budget Summary

```
═══════════════════════════════════════════════════════════════════════
                    SUPRAX PREDICTION COMPLEX
═══════════════════════════════════════════════════════════════════════

BRANCH PREDICTOR (TAGE-SC-L): 2.2M transistors
├── TAGE Base (12K entries)           1,850K
│   ├── 8 tables × 1.5K entries
│   ├── 24 bits per entry
│   ├── Geometric history [0,4,8,12,16,24,32,64]
│   └── Context tags (Spectre immunity)
├── RAS (32 entries)                     20K
│   └── Call/return stack
├── Loop Predictor (384 entries)        100K
│   └── Iteration counting
├── Statistical Corrector (3×1.5K)      250K
│   └── TAGE bias correction
└── Arbiter                              15K

Expected accuracy: 91-93%

───────────────────────────────────────────────────────────────────────

PREFETCH PREDICTOR: 200K transistors
├── Stride Detector (512 entries)        60K
│   ├── Per-PC stride tracking
│   └── Fixed delta patterns (~60% coverage)
├── Delta Pattern (1024 entries)        100K
│   ├── 4-delta shift register per entry
│   └── Repeating sequences (~20% coverage)
├── Stream Detector (16 entries)         20K
│   └── Sequential cache line access (~15% coverage)
└── Arbiter + Throttle                   20K
    ├── Priority merging
    └── Accuracy-based throttling

Expected coverage: 80-88%

═══════════════════════════════════════════════════════════════════════
TOTAL: 2.4M transistors

COMPARISON:
├── Intel branch predictor alone:    ~22M transistors
├── AMD prefetch predictor alone:    ~5-10M transistors
├── SUPRAX branch + prefetch:        2.4M transistors
└── Ratio: ~10x smaller than Intel branch alone
═══════════════════════════════════════════════════════════════════════
```

---

This completes the specification. The next Claude should have everything needed to implement the prefetch engine using the same architectural patterns as the arbitrage system.