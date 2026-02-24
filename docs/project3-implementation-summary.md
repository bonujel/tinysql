# TinySQL Project 3 实现总结

## 项目概述

Project 3 实现了 TinySQL 的 DDL（Data Definition Language）功能，基于 Google F1 的异步 schema 变更算法，支持在线添加和删除列操作。

## Project 3 考察点与学习目标

### 核心考察点

#### 1. 分布式系统中的在线 Schema 变更

**考察内容：**
- 理解为什么传统的 Schema 变更方式（锁表）在分布式系统中不可行
- 掌握 Google F1 异步 Schema 变更算法的核心思想
- 理解如何在不停机的情况下安全地修改数据库结构

**需要掌握的知识：**
- **两阶段提交的局限性**：在分布式环境中，锁表会导致长时间的服务不可用
- **Schema 版本管理**：不同节点可能运行不同版本的 Schema，需要确保兼容性
- **状态机设计**：通过多个中间状态确保 Schema 变更的安全性

**实际应用场景：**
```
场景：在线电商系统需要给订单表添加新字段
传统方式：锁表 → 添加列 → 解锁（服务中断数分钟）
F1 方式：通过状态机逐步添加，服务全程可用
```

#### 2. 状态机设计与实现

**考察内容：**
- 设计合理的状态转换流程
- 理解每个状态的语义和作用
- 掌握状态转换的时机和条件

**需要掌握的知识：**

**状态机详解：**
```
StateNone (不存在)
  ↓ 创建列元数据，调整位置
StateDeleteOnly (仅删除)
  - 新列对读操作不可见
  - 删除操作需要处理新列
  - 目的：确保旧版本代码不会读到新列
  ↓
StateWriteOnly (仅写入)
  - 新列对读操作仍不可见
  - 写入操作需要填充新列
  - 目的：开始积累新列的数据
  ↓
StateWriteReorganization (写入重组)
  - 回填历史数据到新列
  - 新列对读操作仍不可见
  - 目的：确保所有行都有新列的值
  ↓
StatePublic (公开)
  - 新列对所有操作可见
  - Schema 变更完成
```

**关键理解：**
- 为什么需要 DeleteOnly 状态？防止旧版本代码读到不完整的数据
- 为什么需要 WriteOnly 状态？让新版本代码开始写入，但不影响旧版本读取
- 为什么需要 Reorganization 状态？回填历史数据，确保数据完整性

#### 3. 并发控制与数据一致性

**考察内容：**
- 理解 DDL 与 DML 的并发执行问题
- 掌握如何在状态转换过程中保持数据一致性
- 理解 Schema 版本同步机制

**需要掌握的知识：**

**并发场景分析：**
```go
// 场景 1：DDL 正在添加列（StateWriteOnly），DML 插入新行
// 问题：新行需要包含新列的值吗？
// 答案：需要，因为已经进入 WriteOnly 状态

// 场景 2：DDL 正在删除列（StateDeleteOnly），DML 查询数据
// 问题：查询结果应该包含被删除的列吗？
// 答案：不应该，因为已经进入 DeleteOnly 状态

// 场景 3：多个节点运行不同版本的 Schema
// 问题：如何确保数据一致性？
// 答案：通过版本号和状态机确保兼容性
```

**版本同步机制：**
- `updateVersionAndTableInfo`：更新 Schema 版本并通知所有节点
- `waitSchemaChanged`：等待所有节点同步到最新版本
- 版本号单调递增，确保顺序性

#### 4. 数据结构设计与内存管理

**考察内容：**
- 理解 `TableInfo`、`ColumnInfo` 等核心数据结构
- 掌握列的物理位置（数组索引）与逻辑位置（offset）的区别
- 理解 offset 调整的时机和方法

**需要掌握的知识：**

**核心数据结构：**
```go
type TableInfo struct {
    ID      int64
    Name    string
    Columns []*ColumnInfo  // 列的物理存储
    Indices []*IndexInfo
    // ...
}

type ColumnInfo struct {
    ID     int64
    Name   string
    Offset int            // 列的逻辑位置
    State  SchemaState    // 列的状态
    // ...
}
```

**Offset 管理的复杂性：**
```
物理位置 vs 逻辑位置：
- 物理位置：列在 Columns 数组中的索引
- 逻辑位置：列的 Offset 字段，用于数据编码/解码

为什么需要区分？
- 添加列时，先追加到数组末尾（物理位置）
- 然后调整 Offset 到目标位置（逻辑位置）
- 这样可以避免移动数组元素，提高效率

调整时机的重要性：
- 过早调整：可能导致旧版本代码访问错误
- 过晚调整：可能导致中间状态不一致
- 正确时机：StateNone → StateDeleteOnly 转换时
```

#### 5. 错误处理与回滚机制

**考察内容：**
- 理解 DDL 操作失败时的回滚策略
- 掌握如何取消正在进行的 DDL 操作
- 理解 Job 状态管理

**需要掌握的知识：**

**Job 状态管理：**
```go
type Job struct {
    ID          int64
    Type        ActionType      // DDL 操作类型
    State       JobState        // Job 状态
    SchemaState SchemaState     // Schema 状态
    Args        []interface{}   // 操作参数
    // ...
}

// Job 状态
const (
    JobStateNone       // 初始状态
    JobStateRunning    // 运行中
    JobStateDone       // 完成
    JobStateCancelled  // 已取消
    JobStateRollingback // 回滚中
    JobStateRollbackDone // 回滚完成
)
```

**回滚机制：**
```go
// 添加列的回滚 = 删除列
if job.IsRollingback() {
    ver, err = onDropColumn(t, job)
    if err != nil {
        return ver, errors.Trace(err)
    }
    return ver, nil
}
```

#### 6. 测试驱动开发与调试技巧

**考察内容：**
- 通过测试理解需求和预期行为
- 掌握如何分析测试失败原因
- 学会使用日志和调试工具定位问题

**需要掌握的知识：**

**测试分析方法：**
```go
// 1. 理解测试的期望行为
func (s *testColumnSuite) checkPublicColumn(...) {
    updatedRow := append(oldRow, types.NewDatum(columnValue))
    // 期望：新列追加到末尾
}

// 2. 分析测试失败的原因
// 期望：[1, 2, 3, 4]
// 实际：[4, 1, 2, 3]
// 结论：新列被插入到开头而不是末尾

// 3. 追踪问题根源
// 检查 offset 的值和使用方式
// 检查 adjustColumnInfoInAddColumn 的调用时机
```

**调试技巧：**
- 添加日志输出关键变量
- 分析 panic 堆栈定位问题
- 使用测试隔离问题范围

### 学习目标

完成 Project 3 后，你应该能够：

#### 理论层面
1. ✅ 解释为什么分布式数据库需要异步 Schema 变更
2. ✅ 描述 F1 算法的核心思想和状态转换流程
3. ✅ 分析不同状态下 DDL 与 DML 的并发行为
4. ✅ 理解 Schema 版本管理的重要性

#### 实践层面
1. ✅ 实现基于状态机的 DDL 操作（添加列、删除列）
2. ✅ 正确处理列的 offset 管理和位置调整
3. ✅ 确保 DDL 操作的并发安全性
4. ✅ 实现 DDL 操作的错误处理和回滚

#### 工程能力
1. ✅ 阅读和理解大型代码库
2. ✅ 通过测试驱动开发定位和修复问题
3. ✅ 使用日志和调试工具分析复杂问题
4. ✅ 编写清晰的代码注释和文档

### 扩展思考

完成 Project 3 后，可以思考以下问题：

1. **性能优化**
   - 如何减少状态转换的次数？
   - 如何优化 Schema 版本同步的开销？
   - 大表添加列时如何避免长时间锁定？

2. **功能扩展**
   - 如何支持修改列类型？
   - 如何支持添加索引？
   - 如何支持重命名列？

3. **容错性**
   - DDL 操作执行到一半时节点崩溃怎么办？
   - 如何确保 DDL 操作的幂等性？
   - 如何处理网络分区导致的版本不一致？

4. **与其他系统的对比**
   - MySQL 的 Online DDL 与 F1 算法有什么区别？
   - PostgreSQL 如何处理 Schema 变更？
   - 其他分布式数据库（如 CockroachDB）如何实现？

## 核心技术难点

### 1. 异步 Schema 变更状态机

**技术挑战：**
- 需要实现 F1 论文中描述的多状态转换机制
- 确保在状态转换过程中数据库始终可用
- 保证不同状态下的数据一致性

**状态转换流程：**
```
StateNone → StateDeleteOnly → StateWriteOnly → StateWriteReorganization → StatePublic
```

**关键实现点：**
- 每个状态转换都需要更新 schema 版本
- 使用 `updateVersionAndTableInfo` 确保版本同步
- 在 StateDeleteOnly 阶段就需要调整列位置，而不是等到最后

### 2. 列位置（Offset）管理

**技术挑战：**
这是 Project 3 中最复杂的技术难点，涉及多个层面的 offset 处理。

**问题根源：**
1. `createColumnInfo` 将新列追加到 `tblInfo.Columns` 末尾，设置 `colInfo.Offset = len(cols)`
2. Job 参数中的 offset 可能与 colInfo.Offset 不同（例如 offset=0 表示使用默认位置）
3. 需要在正确的时机调用 `adjustColumnInfoInAddColumn` 调整列位置

**Offset 语义：**
- `offset = 0`：使用 `columnInfo.Offset` 的值（追加到末尾）
- `offset != 0`：插入到指定位置

### 3. 并发安全性

**技术挑战：**
- DDL 操作与 DML 操作并发执行
- 多个 goroutine 可能同时访问表结构
- 需要确保 offset 在所有状态下都是一致的

**解决方案：**
在 StateNone → StateDeleteOnly 转换时立即调整列位置，避免中间状态的 offset 不一致。

## 实现过程中遇到的问题

### 问题 1: Index Out of Range Panic

**错误信息：**
```
panic: runtime error: index out of range [4] with length 4
at expression.go:521
```

**问题分析：**
1. 新列在 `createColumnInfo` 中被追加到 `tblInfo.Columns` 末尾（位置 3）
2. 但 `columnInfo.Offset` 可能被设置为其他值（例如 0）
3. 当其他代码使用 `columnInfo.Offset` 访问数组时，会访问错误的位置
4. 如果在 StateWriteReorganization 状态才调整位置，中间状态会出现不一致

**解决方案：**
```go
case model.StateNone:
    // none -> delete only
    // Adjust column position immediately after adding to ensure offset consistency
    adjustColumnInfoInAddColumn(tblInfo, offset)
    job.SchemaState = model.StateDeleteOnly
    columnInfo.State = model.StateDeleteOnly
    ver, err = updateVersionAndTableInfoWithCheck(t, job, tblInfo, originalState != columnInfo.State)
```

**关键点：**
- 在首个状态转换时就调整列位置
- 确保整个状态机过程中 offset 始终一致
- 避免并发访问时的索引越界

### 问题 2: Offset 参数处理错误

**错误现象：**
测试期望列顺序为 `[1, 2, 3, 4]`，实际得到 `[4, 1, 2, 3]`

**问题分析：**
1. `buildCreateColumnJob` 创建 job 时：
   - `col.Offset = len(tblInfo.Columns) = 3`（追加到末尾）
   - `Args: []interface{}{col, 0}`（offset 参数为 0）

2. `checkAddColumn` 解析参数：
   - `job.DecodeArgs(col, &offset)` 将 Args[1]=0 解析到 offset 变量
   - 这会覆盖 col.Offset 的值

3. 错误的修复尝试：
   - 最初保留了 `originalOffset = 0`
   - 导致列被插入到位置 0 而不是末尾

**正确的解决方案：**
```go
if columnInfo == nil {
    columnInfo, _, err = createColumnInfo(tblInfo, col)
    if err != nil {
        job.State = model.JobStateCancelled
        return ver, errors.Trace(err)
    }
    // If offset is 0, use the column's offset (append to end)
    // Otherwise, use the specified offset for insertion
    if offset == 0 {
        offset = columnInfo.Offset
    }
    // ... rest of code
}
```

**关键理解：**
- `offset = 0` 是一个特殊值，表示"使用列的默认位置"
- `columnInfo.Offset` 由 `createColumnInfo` 设置为 `len(cols)`（末尾）
- 只有当 `offset != 0` 时才使用指定的插入位置

### 问题 3: Project 6 代码未实现导致测试失败

**错误信息：**
```
panic: YOUR CODE HERE
at snapshot.go:150
```

**问题分析：**
- Project 3 测试触发了 Project 6 的代码路径
- snapshot.go 中有未实现的代码占位符

**临时解决方案：**
```go
// store/tikv/snapshot.go:150
if keyErr != nil {
    return nil, errors.Errorf("key error: %v", keyErr)
}
```

**说明：**
这是一个临时修复，允许 Project 3 测试正常运行。在实现 Project 6 时需要正确处理这个逻辑。

## 核心代码修改

### 1. ddl/column.go - onAddColumn 函数

**修改位置：** 177-197 行

**修改内容：**
```go
if columnInfo == nil {
    columnInfo, _, err = createColumnInfo(tblInfo, col)
    if err != nil {
        job.State = model.JobStateCancelled
        return ver, errors.Trace(err)
    }
    // If offset is 0, use the column's offset (append to end)
    // Otherwise, use the specified offset for insertion
    if offset == 0 {
        offset = columnInfo.Offset
    }
    logutil.BgLogger().Info("[ddl] run add column job",
        zap.String("job", job.String()),
        zap.Reflect("columnInfo", *columnInfo),
        zap.Int("offset", offset))
    // Set offset arg to job.
    if offset != 0 {
        job.Args = []interface{}{columnInfo, offset}
    }
    if err = checkAddColumnTooManyColumns(len(tblInfo.Columns)); err != nil {
        job.State = model.JobStateCancelled
        return ver, errors.Trace(err)
    }
}
```

### 2. ddl/column.go - 状态机实现

**修改位置：** 202-230 行

**关键修改：**
```go
case model.StateNone:
    // none -> delete only
    // Adjust column position immediately after adding to ensure offset consistency
    adjustColumnInfoInAddColumn(tblInfo, offset)
    job.SchemaState = model.StateDeleteOnly
    columnInfo.State = model.StateDeleteOnly
    ver, err = updateVersionAndTableInfoWithCheck(t, job, tblInfo, originalState != columnInfo.State)
```

**重要变更：**
- 在 StateNone → StateDeleteOnly 转换时调整列位置
- 移除了 StateWriteReorganization → StatePublic 时的位置调整
- 确保整个状态机过程中 offset 一致

## 测试结果

所有 Project 3 测试通过：
- ✅ TestAddColumn
- ✅ TestDropColumn
- ✅ TestColumnChange

## 经验总结

### 1. 理解 F1 异步 Schema 变更算法

**关键点：**
- 状态机设计确保了在线 DDL 的安全性
- 每个状态转换都有明确的语义
- 需要在正确的时机执行关键操作

### 2. Offset 管理的复杂性

**教训：**
- 不要假设 offset 的语义，需要仔细分析代码逻辑
- offset=0 可能是特殊值，不一定表示位置 0
- 列的物理位置（数组索引）和逻辑位置（offset）可能不同

### 3. 并发安全的重要性

**原则：**
- 在首个状态转换时就建立一致性
- 避免中间状态的不一致
- 考虑并发访问的场景

### 4. 测试驱动开发

**实践：**
- 通过测试发现问题
- 分析测试期望理解正确行为
- 逐步修复直到所有测试通过

## 参考资料

1. Google F1 论文：Online, Asynchronous Schema Change in F1
2. TinySQL 项目文档
3. TiDB DDL 实现源码

## 附录：调试技巧

### 1. 使用日志追踪

```go
logutil.BgLogger().Info("[ddl] run add column job",
    zap.String("job", job.String()),
    zap.Reflect("columnInfo", *columnInfo),
    zap.Int("offset", offset))
```

### 2. 理解测试期望

查看测试代码中的断言：
```go
updatedRow := append(oldRow, types.NewDatum(columnValue))
if !reflect.DeepEqual(data, updatedRow) {
    return false, errors.Errorf("%v not equal to %v", data, updatedRow)
}
```

### 3. 分析 Panic 堆栈

```
panic: runtime error: index out of range [4] with length 4
at expression.go:521
```

找到具体的代码位置，理解为什么会访问越界的索引。

---

**文档版本：** 1.0
**最后更新：** 2026-02-24
**作者：** Kiro AI Assistant
