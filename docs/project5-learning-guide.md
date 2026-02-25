# TinySQL Project 5 学习指南：执行层性能优化

## 项目概述

Project 5 聚焦于数据库执行层的性能优化，通过三个递进的实践任务，让你深入理解现代数据库如何通过并发和向量化技术提升查询性能。这不是简单的功能实现，而是对数据库核心执行机制的深度探索。

## 核心学习目标

完成这个项目后，你应该能够：

1. **理解执行模型的演进**：从传统的 Volcano 模型到向量化执行，认识到为什么批处理比逐行处理更高效
2. **掌握并发编程模式**：在 Go 语言中实现生产者-消费者模式，理解 channel、goroutine 和 WaitGroup 的协作机制
3. **设计并发算法**：将串行算法改造为并发版本，处理数据分区、任务分发和结果合并
4. **处理并发同步问题**：识别和解决常见的并发 bug，如 WaitGroup 计数错误、channel 死锁等

## Part 1：向量化执行 - 批处理的威力

### 问题背景

传统的 Volcano 模型采用"拉取"方式逐行处理数据：
```
for row := range input {
    if condition(row) {
        output <- row
    }
}
```

这种方式简单直观，但存在严重的性能问题：
- 每处理一行数据都要调用一次函数
- 函数调用开销（栈帧创建、参数传递、返回值处理）累积起来非常可观
- CPU 分支预测失效率高
- 无法利用 SIMD 指令

### 向量化的核心思想

向量化执行将数据组织成批次（Chunk），一次处理多行：
```
chunk := getNextChunk()  // 获取 1024 行
results := evaluateOnChunk(chunk)  // 批量计算
```

**性能提升的来源：**
1. 函数调用次数从 N 次降低到 N/1024 次
2. 循环展开和 CPU 流水线优化的机会增多
3. 为 SIMD 优化打下基础（虽然 TinySQL 未实现）

### 实现要点

**vecEvalInt 实现：**
```go
func (b *builtinLengthSig) vecEvalInt(input *chunk.Chunk, result *chunk.Column) error {
    n := input.NumRows()

    // 1. 批量获取输入字符串
    buf, err := b.bufAllocator.get(types.ETString, n)
    if err != nil {
        return err
    }
    defer b.bufAllocator.put(buf)

    if err := b.args[0].VecEvalString(b.ctx, input, buf); err != nil {
        return err
    }

    // 2. 预分配结果空间
    result.ResizeInt64(n, false)
    result.MergeNulls(buf)  // 处理 NULL 值

    // 3. 批量计算长度
    i64s := result.Int64s()
    for i := 0; i < n; i++ {
        if result.IsNull(i) {
            continue
        }
        i64s[i] = int64(len([]byte(buf.GetString(i))))
    }
    return nil
}
```

**关键技术点：**
- **内存复用**：通过 bufAllocator 复用临时缓冲区，减少 GC 压力
- **NULL 处理**：使用位图（bitmap）高效标记 NULL 值
- **批量操作**：一次性分配结果空间，避免动态扩容

**Selection 向量化：**
```go
func (e *SelectionExec) Next(ctx context.Context, req *chunk.Chunk) error {
    req.Reset()
    for {
        err := Next(ctx, e.children[0], e.childChunk)
        if err != nil || e.childChunk.NumRows() == 0 {
            return err
        }

        // 使用向量化过滤器
        selected, err := expression.VectorizedFilter(e.ctx, e.filters,
            chunk.NewIterator4Chunk(e.childChunk), req)
        if err != nil {
            return err
        }

        if selected > 0 {
            return nil  // 有数据就返回
        }
        // 没有数据继续读取下一批
    }
}
```

### 技术延伸

**1. 列式存储与向量化的关系**

向量化执行与列式存储是天然的搭配：
- 列式存储将同一列的数据连续存放，便于批量读取
- 向量化执行一次处理一列的多个值，访问模式匹配
- 现代分析型数据库（如 ClickHouse、DuckDB）都采用这种组合

**2. SIMD 优化**

向量化为 SIMD（Single Instruction Multiple Data）优化创造了条件：
```
// 传统方式：逐个比较
for i := 0; i < n; i++ {
    result[i] = (a[i] > b[i])
}

// SIMD 方式：一条指令比较 8 个值
__m256i va = _mm256_loadu_si256(a);
__m256i vb = _mm256_loadu_si256(b);
__m256i vr = _mm256_cmpgt_epi32(va, vb);
```

**3. JIT 编译**

更激进的优化是运行时编译（JIT），根据查询动态生成机器码：
- 消除解释开销
- 针对具体数据类型优化
- 代表：HyPer、Peloton

## Part 2：Hash Join - 并发的艺术

### 问题背景

Join 是数据库中最昂贵的操作之一。对于两个表的 Join：
```sql
SELECT * FROM orders JOIN customers ON orders.customer_id = customers.id
```

传统的嵌套循环 Join 复杂度是 O(M×N)，当数据量大时性能不可接受。

### Hash Join 原理

Hash Join 将 Join 分为两个阶段：

**Build 阶段：**
1. 读取内表（较小的表）
2. 对 Join 键计算哈希值
3. 构建哈希表：hash(key) -> rows

**Probe 阶段：**
1. 读取外表（较大的表）
2. 对每行的 Join 键计算哈希值
3. 在哈希表中查找匹配的行
4. 输出匹配结果

复杂度降低到 O(M+N)。

### 并发 Hash Join 设计

TinySQL 的并发模型：

```
                    ┌─────────────┐
                    │ Main Thread │
                    └──────┬──────┘
                           │
              ┌────────────┼────────────┐
              ▼            ▼            ▼
         ┌────────┐  ┌────────┐  ┌────────┐
         │ Inner  │  │ Outer  │  │ Join   │
         │Fetcher │  │Fetcher │  │Workers │
         └────┬───┘  └────┬───┘  └───┬────┘
              │           │           │
              ▼           │           │
         ┌─────────┐      │           │
         │  Hash   │      │           │
         │  Table  │◄─────┴───────────┘
         └─────────┘
```

**Inner Fetcher：**
- 单线程读取内表数据
- 构建哈希表
- 完成后通知 Outer Fetcher 和 Join Workers

**Outer Fetcher：**
- 单线程读取外表数据
- 通过 channel 分发给多个 Join Workers

**Join Workers：**
- 多个 goroutine 并发执行
- 从 channel 接收外表数据
- 在哈希表中查找匹配
- 将结果发送到结果 channel

### 实现要点

**fetchAndBuildHashTable：**
```go
func (e *HashJoinExec) fetchAndBuildHashTable(ctx context.Context) error {
    // 1. 创建哈希上下文
    hCtx := &hashContext{
        allTypes:  e.innerSideExec.retTypes(),
        keyColIdx: e.innerSideKeyColIdx,
    }

    // 2. 创建哈希表容器
    e.rowContainer = newHashRowContainer(e.ctx, int(e.innerSideEstCount), hCtx)

    // 3. 读取所有内表数据并插入哈希表
    for {
        chk := e.innerSideExec.newFirstChunk()
        err := Next(ctx, e.innerSideExec, chk)
        if err != nil || chk.NumRows() == 0 {
            return err
        }

        if err = e.rowContainer.PutChunk(chk); err != nil {
            return err
        }
    }
}
```

**runJoinWorker：**
```go
func (e *HashJoinExec) runJoinWorker(workerID uint, outerKeyColIdx []int) {
    defer e.joinWorkerWaitGroup.Done()

    hCtx := &hashContext{
        allTypes:  e.outerSideExec.base().retFieldTypes,
        keyColIdx: outerKeyColIdx,
    }

    for {
        // 1. 获取结果 chunk
        ok, joinResult := e.getNewJoinResult(workerID)
        if !ok {
            return
        }

        // 2. 接收外表数据
        select {
        case <-e.closeCh:
            return
        case outerChk, ok := <-e.outerResultChs[workerID]:
            if !ok {
                return
            }
        }

        // 3. Join 操作
        ok, joinResult = e.join2Chunk(workerID, outerChk, hCtx,
            joinResult, selected)
        if !ok {
            return
        }

        // 4. 发送结果
        e.joinResultCh <- joinResult
    }
}
```

### 并发同步的陷阱

**WaitGroup 计数错误：**

最初的实现中遇到了 "negative WaitGroup counter" 错误。原因是：
```go
// 错误的做法
func worker() {
    defer handlePanic()  // panic handler 中调用 Done()
    defer wg.Done()      // 正常退出也调用 Done()
    // ... 工作代码
}
```

两个 defer 都会调用 `Done()`，导致计数错误。

**正确的做法：**
```go
func worker() {
    defer wg.Done()  // 只在这里调用一次
    defer handlePanic()  // panic handler 不调用 Done()
    // ... 工作代码
}
```

**关键原则：**
- 每个 goroutine 的 `Done()` 调用次数必须与 `Add()` 次数严格匹配
- 使用 defer 确保无论如何退出都会调用 `Done()`
- panic handler 不应该重复调用 `Done()`

### 技术延伸

**1. Grace Hash Join**

当内表太大无法放入内存时，使用 Grace Hash Join：
1. 将两个表按相同的哈希函数分区到磁盘
2. 逐个分区进行 Join
3. 保证相同的 Join 键在同一分区

**2. Bloom Filter 优化**

在 Probe 阶段前使用 Bloom Filter 快速过滤：
```go
// Build 阶段构建 Bloom Filter
for key := range innerTable {
    bloomFilter.Add(key)
}

// Probe 阶段先检查
for row := range outerTable {
    if !bloomFilter.MayContain(row.key) {
        continue  // 快速跳过不匹配的行
    }
    // 再查哈希表
}
```

**3. 分区并行**

更激进的并行策略是分区并行：
- 将内表和外表都按哈希分区
- 每个分区独立进行 Join
- 完全避免锁竞争

## Part 3：Hash Aggregate - 分而治之

### 问题背景

聚合操作（如 SUM、AVG、COUNT）需要扫描大量数据。串行执行时，单线程成为瓶颈。

### 并发聚合的挑战

聚合操作的特点：
- 需要维护状态（如累加和、计数）
- 相同 Group Key 的数据必须聚合到一起
- 最终结果需要合并

如何并行化？关键是利用聚合函数的**结合律**和**交换律**：
```
SUM(a, b, c, d) = SUM(SUM(a, b), SUM(c, d))
```

### 两阶段聚合模型

**Partial 阶段（预聚合）：**
- 多个 Partial Workers 并发读取数据
- 各自计算部分聚合结果
- 输出中间结果

**Final 阶段（合并）：**
- 多个 Final Workers 接收中间结果
- 合并得到最终结果
- 输出给用户

**关键问题：如何保证相同 Group Key 的中间结果到达同一个 Final Worker？**

答案：哈希分发（Shuffle）

### Shuffle 机制

```go
func (w *HashAggPartialWorker) shuffleIntermData(sc *stmtctx.StatementContext,
    finalConcurrency int) {
    // 1. 按哈希值分组
    groupKeysMap := make(map[int64][]string, finalConcurrency)
    for groupKey := range w.partialResultsMap {
        finalWorkerIdx := int(murmur3.Sum32([]byte(groupKey))) % finalConcurrency
        groupKeysMap[int64(finalWorkerIdx)] = append(
            groupKeysMap[int64(finalWorkerIdx)], groupKey)
    }

    // 2. 发送给对应的 Final Worker
    for finalWorkerIdx, groupKeys := range groupKeysMap {
        intermData := &HashAggIntermData{
            groupKeys:        groupKeys,
            partialResultMap: make(aggPartialResultMapper, len(groupKeys)),
        }
        for _, groupKey := range groupKeys {
            intermData.partialResultMap[groupKey] = w.partialResultsMap[groupKey]
        }
        w.outputChs[finalWorkerIdx] <- intermData
    }
}
```

**哈希分发的保证：**
- 相同的 Group Key 计算出相同的哈希值
- 相同的哈希值模运算后得到相同的 Worker ID
- 因此相同的 Group Key 一定发送给同一个 Final Worker

### Final Worker 合并

```go
func (w *HashAggFinalWorker) consumeIntermData(sctx sessionctx.Context) error {
    for {
        intermData, ok := w.getPartialInput()
        if !ok {
            return nil
        }

        for _, groupKey := range intermData.groupKeys {
            // 1. 记录 Group Key
            if !w.groupSet.Exist(groupKey) {
                w.groupSet.Insert(groupKey)
            }

            // 2. 获取或创建最终结果
            finalPartialResults, exists := w.partialResultMap[groupKey]
            if !exists {
                finalPartialResults = make([]aggfuncs.PartialResult, len(w.aggFuncs))
                for i, af := range w.aggFuncs {
                    finalPartialResults[i] = af.AllocPartialResult()
                }
                w.partialResultMap[groupKey] = finalPartialResults
            }

            // 3. 合并中间结果
            intermPartialResults := intermData.partialResultMap[groupKey]
            for i, af := range w.aggFuncs {
                if err := af.MergePartialResult(sctx, intermPartialResults[i],
                    finalPartialResults[i]); err != nil {
                    return err
                }
            }
        }
    }
}
```

### 聚合函数的模式

TinySQL 定义了四种聚合模式：

| 模式 | 输入 | 输出 | 用途 |
|------|------|------|------|
| CompleteMode | 原始数据 | 最终结果 | 单阶段聚合 |
| Partial1Mode | 原始数据 | 中间结果 | 第一次预聚合 |
| Partial2Mode | 中间结果 | 中间结果 | 多级预聚合 |
| FinalMode | 中间结果 | 最终结果 | 最终合并 |

**示例：AVG 函数的分解**

AVG 不能直接合并，需要分解为 SUM 和 COUNT：
```
Partial: AVG(1,2,3,4) -> (SUM=10, COUNT=4)
Partial: AVG(5,6,7,8) -> (SUM=26, COUNT=4)
Final:   Merge -> (SUM=36, COUNT=8) -> AVG=4.5
```

### 技术延伸

**1. 自适应并发度**

根据数据特征动态调整 Worker 数量：
- Group Key 数量少：减少 Final Workers，避免空转
- Group Key 数量多：增加 Final Workers，提高并行度
- 数据倾斜：动态重新分区

**2. 增量聚合**

流式场景下的增量聚合：
```go
// 维护滑动窗口的聚合状态
type IncrementalAgg struct {
    window    []Row
    aggState  PartialResult
}

func (a *IncrementalAgg) Update(newRow Row) {
    // 添加新数据
    a.aggState.Add(newRow)
    a.window = append(a.window, newRow)

    // 移除过期数据
    if len(a.window) > windowSize {
        oldRow := a.window[0]
        a.aggState.Remove(oldRow)
        a.window = a.window[1:]
    }
}
```

**3. 近似聚合**

对于超大规模数据，使用近似算法：
- HyperLogLog：近似 COUNT DISTINCT
- T-Digest：近似分位数
- Count-Min Sketch：近似频率统计

## 性能对比与思考

### 向量化的收益

理论分析：
- 函数调用开销：约 10-20 纳秒/次
- 处理 100 万行数据：
  - 逐行：100 万次调用 = 10-20 毫秒
  - 向量化（1024 批次）：约 1000 次调用 = 0.01-0.02 毫秒

实际收益取决于：
- 表达式复杂度（越复杂收益越大）
- 数据类型（定长类型更友好）
- CPU 缓存命中率

### 并发的收益与代价

**收益：**
- 理想情况下，N 个 Worker 可以达到 N 倍加速
- 充分利用多核 CPU

**代价：**
- 线程创建和销毁开销
- 上下文切换开销
- 同步和通信开销
- 内存占用增加

**最佳实践：**
- Worker 数量 = CPU 核心数（CPU 密集型任务）
- Worker 数量 = 2-4 倍 CPU 核心数（IO 密集型任务）
- 避免过度并发导致的上下文切换

### 何时不应该并发

1. **数据量太小**：并发开销大于收益
2. **任务太轻**：单个任务执行时间极短
3. **强依赖关系**：任务之间有严格的顺序要求
4. **内存受限**：并发会导致内存不足

## 实战建议

### 调试并发程序

**1. 使用 Go 的竞态检测器：**
```bash
go test -race ./executor
```

**2. 添加日志跟踪：**
```go
log.Printf("[Worker %d] Processing chunk with %d rows", workerID, chunk.NumRows())
```

**3. 使用 pprof 分析性能：**
```go
import _ "net/http/pprof"

go func() {
    http.ListenAndServe("localhost:6060", nil)
}()
```

访问 `http://localhost:6060/debug/pprof/` 查看性能数据。

### 常见错误模式

**1. Channel 死锁：**
```go
// 错误：没有接收者
ch := make(chan int)
ch <- 1  // 永久阻塞

// 正确：使用 buffered channel 或启动接收 goroutine
ch := make(chan int, 1)
ch <- 1
```

**2. Goroutine 泄漏：**
```go
// 错误：goroutine 永远不会退出
go func() {
    for {
        select {
        case data := <-ch:
            process(data)
        }
    }
}()

// 正确：添加退出条件
go func() {
    for {
        select {
        case data := <-ch:
            process(data)
        case <-ctx.Done():
            return
        }
    }
}()
```

**3. 共享状态竞争：**
```go
// 错误：多个 goroutine 修改共享变量
var counter int
for i := 0; i < 10; i++ {
    go func() {
        counter++  // 竞态条件
    }()
}

// 正确：使用 atomic 或 mutex
var counter int64
for i := 0; i < 10; i++ {
    go func() {
        atomic.AddInt64(&counter, 1)
    }()
}
```

## 总结

Project 5 通过三个递进的任务，让你掌握了数据库执行层优化的核心技术：

1. **向量化执行**：理解批处理的价值，学会设计向量化算子
2. **并发 Hash Join**：掌握并发算法设计，处理复杂的同步问题
3. **并发聚合**：理解分布式计算的基本模式（Map-Reduce）

这些技术不仅适用于数据库，也是分布式系统、大数据处理的基础。掌握这些技术后，你可以：
- 设计高性能的数据处理系统
- 理解现代数据库（如 ClickHouse、DuckDB）的实现原理
- 在面试中深入讨论数据库内核技术

继续深入的方向：
- 阅读 ClickHouse、DuckDB 的源码
- 学习 SIMD 编程和 JIT 编译
- 研究分布式查询执行（如 Spark、Presto）
- 探索 GPU 加速的数据库（如 OmniSci）

记住：性能优化永远是在理解原理的基础上，针对具体场景做权衡。没有银弹，只有合适的方案。
