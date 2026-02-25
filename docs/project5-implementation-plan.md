# Project 5 执行层实现计划

## 项目概述

Project 5 是 TinySQL 的执行层实现,包含三个核心部分:
- **Part 1 (20%)**: 执行模型 - 火山模型和向量化
- **Part 2 (40%)**: Hash Join 并发实现
- **Part 3 (40%)**: Hash Aggregate 并发实现

## Part 1: 执行模型与向量化 (20%)

### 1.1 理论基础

#### 火山模型 (Volcano Model)
- **核心思想**: 每个执行器实现三个接口
  - `Open()`: 初始化资源
  - `Next()`: 返回一条结果
  - `Close()`: 释放资源
- **优点**: 模块化,易于组合
- **缺点**: 函数调用开销大

#### 向量化执行
- **核心思想**: 每次 `Next()` 返回一批数据(Chunk)
- **优点**:
  - 减少函数调用开销
  - 支持 SIMD 向量化计算
  - 减少内存分配
- **实现**: 使用 `Chunk` 数据结构批量处理

### 1.2 实现任务

#### 任务 1.1: 实现向量化表达式 `vecEvalInt`
**文件**: `expression/builtin_string_vec.go:89`

**需求**:
- 实现 `builtinLengthSig.vecEvalInt()` 方法
- 计算字符串长度的向量化版本
- 将 `vectorized()` 返回值改为 `true`

**实现要点**:
1. 从 input Chunk 中获取字符串列
2. 批量计算每个字符串的长度
3. 将结果写入 result Column
4. 处理 NULL 值

**参考**:
- 查看同文件中其他向量化函数实现
- 理解 Chunk 和 Column 的数据结构

#### 任务 1.2: 实现 Selection 的向量化 Next
**文件**: `executor/executor.go:380`

**需求**:
- 实现 `SelectionExec.Next()` 的向量化版本
- 批量过滤数据行

**实现要点**:
1. 循环读取子节点数据直到填满 req 或数据耗尽
2. 使用向量化表达式批量计算过滤条件
3. 根据过滤结果选择性复制行到输出 Chunk
4. 处理边界情况(Chunk 满、数据耗尽)

**算法流程**:
```
while req 未满 and 有数据:
    1. 从子节点读取一个 Chunk
    2. 向量化计算过滤条件 -> selected[]
    3. 将 selected[i]=true 的行复制到 req
    4. 如果 req 满了则退出
```

### 1.3 测试验证
- 运行 `make test-proj5-1`
- 通过 `expression` 包下所有测试
- 通过 `executor/executor_test.go` 中的 `TestSelectExec`

---

## Part 2: Hash Join 并发实现 (40%)

### 2.1 理论基础

#### Hash Join 算法
1. **构建阶段**: 读取内表数据,构造哈希表
2. **探测阶段**: 读取外表数据,查哈希表匹配

#### 并发模型
- **Main Thread** (1个):
  - 构造哈希表
  - 启动 workers
  - 返回结果给调用方
- **Outer Fetcher** (1个):
  - 读取外表数据
  - 分发给 Join Workers
- **Join Workers** (多个):
  - 查哈希表
  - 匹配并 Join 数据
  - 返回结果给 Main Thread

#### 数据流设计
```
                    ┌─────────────┐
                    │ Main Thread │
                    └──────┬──────┘
                           │ 1. Build Hash Table
                           │ 2. Start Workers
                           │ 3. Return Results
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   ┌────▼────┐      ┌──────▼──────┐   ┌──────▼──────┐
   │ Outer   │      │ Join Worker │   │ Join Worker │
   │ Fetcher │─────▶│      1      │   │      2      │
   └─────────┘      └─────────────┘   └─────────────┘
        │                  │                  │
        │ outerResultChs   │ joinResultCh     │
        └──────────────────┴──────────────────┘
```

### 2.2 实现任务

#### 任务 2.1: 实现 `fetchAndBuildHashTable`
**文件**: `executor/join.go:148`

**需求**:
- 读取内表所有数据
- 构造哈希表存储在 `e.rowContainer`

**实现要点**:
1. 循环调用 `e.innerSideExec.Next()` 读取内表数据
2. 调用 `newHashRowContainer()` 创建哈希表容器
3. 将每个 Chunk 的数据插入哈希表
4. 处理错误和资源清理

**伪代码**:
```go
func (e *HashJoinExec) fetchAndBuildHashTable(ctx context.Context) error {
    // 1. 创建 hash row container
    e.rowContainer = newHashRowContainer(...)

    // 2. 循环读取内表数据
    for {
        chk := newFirstChunk(e.innerSideExec)
        err := e.innerSideExec.Next(ctx, chk)
        if err != nil {
            return err
        }
        if chk.NumRows() == 0 {
            break
        }

        // 3. 将数据插入哈希表
        err = e.rowContainer.PutChunk(chk)
        if err != nil {
            return err
        }
    }
    return nil
}
```

#### 任务 2.2: 实现 `runJoinWorker`
**文件**: `executor/join.go:243`

**需求**:
- 从 Outer Fetcher 接收外表数据
- 查哈希表匹配
- 将结果发送给 Main Thread

**实现要点**:
1. 循环从 `e.outerResultChs[workerID]` 接收 Outer Chunk
2. 调用 `e.join2Chunk()` 进行 Join 操作
3. 将结果发送到 `e.joinResultCh`
4. 处理 `e.closeCh` 提前终止信号
5. 资源回收(Chunk 复用)

**伪代码**:
```go
func (e *HashJoinExec) runJoinWorker(workerID uint, outerKeyColIdx []int) {
    defer e.joinWorkerWaitGroup.Done()

    for {
        // 1. 获取 Join Result Chunk
        ok, joinResult := e.getNewJoinResult(workerID)
        if !ok {
            return
        }

        // 2. 接收 Outer Chunk
        select {
        case <-e.closeCh:
            return
        case outerChk, ok := <-e.outerResultChs[workerID]:
            if !ok {
                return
            }

            // 3. Join 操作
            err := e.join2Chunk(workerID, outerChk, joinResult.chk, outerKeyColIdx)
            if err != nil {
                joinResult.err = err
            }

            // 4. 发送结果
            e.joinResultCh <- joinResult

            // 5. 回收 Outer Chunk
            e.outerChkResourceCh <- &outerChkResource{
                chk:  outerChk,
                dest: e.outerResultChs[workerID],
            }
        }
    }
}
```

### 2.3 关键数据结构

#### Channel 通信
- `outerResultChs[i]`: Outer Fetcher -> Join Worker i
- `outerChkResourceCh`: Join Worker -> Outer Fetcher (Chunk 复用)
- `joinChkResourceCh[i]`: Main Thread -> Join Worker i (Chunk 复用)
- `joinResultCh`: Join Worker -> Main Thread (结果传递)

### 2.4 测试验证
- 运行 `make test-proj5-2`
- 通过 `join_test.go` 除 `TestJoin` 外的所有测试
- `TestJoin` 依赖 Part 3,最后测试

---

## Part 3: Hash Aggregate 并发实现 (40%)

### 3.1 理论基础

#### Hash Aggregate 算法
- **核心思想**: 使用哈希表按 Group Key 聚合
- **并发优化**:
  - Partial Workers: 预聚合
  - Final Workers: 合并最终结果

#### ���合函数模式
| 模式 | 输入 | 输出 |
|------|------|------|
| CompleteMode | 原始数据 | 最终结果 |
| FinalMode | 中间结果 | 最终结果 |
| Partial1Mode | 原始数据 | 中间结果 |
| Partial2Mode | 中间结果 | 中间结果 |

#### 并发模型
```
                    ┌─────────────┐
                    │ Main Thread │
                    └──────┬──────┘
                           │ 1. Start Workers
                           │ 2. Receive Results
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   ┌────▼────┐      ┌──────▼──────┐   ┌──────▼──────┐
   │  Data   │      │  Partial    │   │  Partial    │
   │ Fetcher │─────▶│  Worker 1   │   │  Worker 2   │
   └─────────┘      └──────┬──────┘   └──────┬──────┘
                           │ shuffle          │
                           │ by hash(group)   │
                    ┌──────▼──────┐   ┌──────▼──────┐
                    │   Final     │   │   Final     │
                    │  Worker 1   │   │  Worker 2   │
                    └──────┬──────┘   └──────┬──────┘
                           │                  │
                           └────────┬─────────┘
                                    │
                            ┌───────▼────────┐
                            │  Main Thread   │
                            └────────────────┘
```

### 3.2 执行流程

#### 5 个阶段
1. **启动**: Main Thread 启动 Data Fetcher, Partial Workers, Final Workers
2. **数据获取**: Data Fetcher 读取子节点数据分发给 Partial Workers
3. **预聚合**: Partial Workers 计算中间结果,按 Group Key shuffle 给 Final Workers
4. **最终聚合**: Final Workers 合并中间结果,发送给 Main Thread
5. **返回结果**: Main Thread 接收并返回最终结果

### 3.3 实现任务

#### 任务 3.1: 实现 `shuffleIntermData`
**文件**: `executor/aggregate.go:355`

**需求**:
- 将 Partial Worker 的中间结果按 Group Key shuffle 给对应的 Final Worker

**实现要点**:
1. 遍历 `w.partialResultMap` 中的所有 Group Key
2. 对每个 Group Key 计算哈希值
3. 根据哈希值确定目标 Final Worker: `finalWorkerID = hash(groupKey) % finalConcurrency`
4. 将中间结果发送到对应 Final Worker 的 channel

**伪代码**:
```go
func (w *HashAggPartialWorker) shuffleIntermData(sc *stmtctx.StatementContext, finalConcurrency int) {
    // 遍历所有 partial results
    for groupKey, partialResults := range w.partialResultMap {
        // 1. 计算目标 Final Worker ID
        finalWorkerID := int(murmur3.Sum32([]byte(groupKey))) % finalConcurrency

        // 2. 构造 IntermData
        intermData := &HashAggIntermData{
            groupKey:       groupKey,
            partialResults: partialResults,
        }

        // 3. 发送到对应的 Final Worker
        select {
        case <-w.finishCh:
            return
        case w.outputChs[finalWorkerID] <- intermData:
        }
    }
}
```

#### 任务 3.2: 实现 `consumeIntermData`
**文件**: `executor/aggregate.go:425`

**需求**:
- Final Worker 接收 Partial Workers 的中间结果并合并

**实现要点**:
1. 循环从 `w.inputCh` 接收 IntermData
2. 对于每个 Group Key,合并 Partial Results
3. 存储到 `w.partialResultMap`
4. 处理完成信号

**伪代码**:
```go
func (w *HashAggFinalWorker) consumeIntermData(sctx sessionctx.Context) (err error) {
    for {
        select {
        case <-w.finishCh:
            return nil
        case intermData, ok := <-w.inputCh:
            if !ok {
                return nil
            }

            // 获取或创建该 Group 的 Final Partial Results
            groupKey := intermData.groupKey
            finalPartialResults, ok := w.partialResultMap[groupKey]
            if !ok {
                // 创建新的 Partial Results
                finalPartialResults = make([]aggfuncs.PartialResult, len(w.aggFuncs))
                for i, aggFunc := range w.aggFuncs {
                    finalPartialResults[i] = aggFunc.AllocPartialResult()
                }
                w.partialResultMap[groupKey] = finalPartialResults
            }

            // 合并 Partial Results
            for i, aggFunc := range w.aggFuncs {
                err = aggFunc.MergePartialResult(sctx, intermData.partialResults[i], finalPartialResults[i])
                if err != nil {
                    return err
                }
            }
        }
    }
}
```

### 3.4 关键数据结构

#### HashAggPartialWorker
- `partialResultMap`: map[groupKey][]PartialResult - 预聚合结果
- `outputChs`: []chan *HashAggIntermData - 发送给 Final Workers

#### HashAggFinalWorker
- `inputCh`: chan *HashAggIntermData - 接收 Partial Workers 数据
- `partialResultMap`: map[groupKey][]PartialResult - 最终聚合结果
- `outputCh`: chan *AfFinalResult - 发送给 Main Thread

#### HashAggIntermData
- `groupKey`: string - Group Key
- `partialResults`: []PartialResult - 中间聚合结果

### 3.5 测试验证
- 运行 `make test-proj5-3`
- 通过 `aggregate_test.go` 所有测试

---

## 实施顺序

### 阶段 1: Part 1 - 向量化基础 (预计 2-3 小时)
1. 理解火山模型和向量化概念
2. 实现 `vecEvalInt` 向量化表达式
3. 实现 `Selection.Next` 向量化执行
4. 运行测试验证

### 阶段 2: Part 2 - Hash Join (预计 4-5 小时)
1. 理解 Hash Join 并发模型和数据流
2. 实现 `fetchAndBuildHashTable` 构造哈希表
3. 实现 `runJoinWorker` 并发探测
4. 调试 channel 通信和资源管理
5. 运行测试验证

### 阶段 3: Part 3 - Hash Aggregate (预计 4-5 小时)
1. 理解 Hash Aggregate 并发模型
2. 实现 `shuffleIntermData` 数据分发
3. 实现 `consumeIntermData` 结果合并
4. 调试聚合函数模式和数据流
5. 运行测试验证

### 阶段 4: 集成测试 (预计 1-2 ��时)
1. 运行完整的 Project 5 测试套件
2. 修复集成问题
3. 性能验证和优化

---

## 关键技术点

### 1. Chunk 数据结构
- 批量存储多行数据
- 列式存储,支持向量化
- 内存复用,减少分配

### 2. Channel 通信模式
- 生产者-消费者模式
- 资源池模式(Chunk 复用)
- 优雅关闭(finishCh/closeCh)

### 3. 并发控制
- WaitGroup 等待 goroutine 完成
- Select 处理多个 channel
- Context 传递取消信号

### 4. 哈希表设计
- 链表法处理冲突
- Group Key 编码
- 哈希函数选择(murmur3)

---

## 注意事项

### 1. 错误处理
- 所有错误必须正确传播
- 使用 defer 确保资源清理
- Channel 关闭顺序很重要

### 2. 并发安全
- 避免数据竞争
- 正确使用 channel 通信
- 注意 goroutine 泄漏

### 3. 性能优化
- Chunk 复用减少内存分配
- 批量处理提高吞吐量
- 并发度配置(concurrency)

### 4. 测试策略
- 单元测试每个函数
- 集成测试完整流程
- 边界情况测试(空数据、大数据)

---

## 参考资料

### 代码文件
- `executor/executor.go` - Selection 执行器
- `executor/join.go` - Hash Join 执行器
- `executor/aggregate.go` - Hash Aggregate 执行器
- `expression/builtin_string_vec.go` - 向量化表达式
- `util/chunk/chunk.go` - Chunk 数据结构

### 文档
- `courses/proj5-part1-README-zh_CN.md` - Part 1 说明
- `courses/proj5-part2-README-zh_CN.md` - Part 2 说明
- `courses/proj5-part3-README-zh_CN.md` - Part 3 说明

### 测试
- `executor/executor_test.go` - Selection 测试
- `executor/join_test.go` - Hash Join 测试
- `executor/aggregate_test.go` - Hash Aggregate 测试
- `expression/*_test.go` - 表达式测试

---

## 评分标准

- **Part 1**: 20% (expression 50% + executor 50%)
- **Part 2**: 40% (按测试通过比例)
- **Part 3**: 40% (按测试通过比例)
- **总分**: 100 分

全部测试通过可获得满分,部分测试未通过按比例扣分。
