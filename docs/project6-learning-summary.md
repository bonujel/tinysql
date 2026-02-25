# TinySQL Project 6 学习总结：Percolator 分布式事务实现

## 一、核心知识点

### 1.1 Percolator 事务模型

Percolator 是 Google 为增量处理设计的分布式事务协议，TiKV 的事务层就是基于这个模型实现的。

**核心思想**：
- 使用两阶段提交（2PC）保证分布式事务的原子性
- 通过时间戳（Timestamp）实现 MVCC（多版本并发控制）
- 采用乐观锁机制，在提交时检测冲突

**关键组件**：
```
Client (TinySQL)
    ↓
TwoPhaseCommitter (2pc.go)
    ↓
TiKV Regions (分布式存储)
```

### 1.2 两阶段提交流程

#### Prewrite 阶段（预写）
```go
// 为每个 key 写入锁和数据
for each key in transaction:
    1. 检查是否存在冲突（其他事务的锁或更新的版本）
    2. 写入 Lock 列（标记为锁定状态）
    3. 写入 Data 列（实际数据，但对其他事务不可见）
```

**Primary Key 的作用**：
- 事务中第一个写入的 key 作为 Primary
- 其他 key 的锁都指向 Primary
- Primary 的状态决定整个事务的状态

#### Commit 阶段（提交）
```go
// 先提交 Primary，再提交 Secondary keys
1. 提交 Primary key：
   - 写入 Write 列（commitTS -> startTS）
   - 删除 Lock 列

2. 提交 Secondary keys（可异步）：
   - 写入 Write 列
   - 删除 Lock 列
```


**为什么要分两阶段？**
- 如果 Prewrite 失败，直接回滚，不影响其他事务
- 如果 Commit Primary 成功，即使后续失败，事务也算成功（可异步完成）
- Primary 作为"决策点"，避免分布式系统中的不一致

### 1.3 锁解析机制

当读操作遇到锁时，需要判断锁的状态：

```go
// 锁的三种状态
1. 活跃锁（TTL > 0）：事务正在进行，需要等待
2. 已提交（commitTS > 0）：事务已提交，可以读取数据
3. 已回滚（TTL = 0, commitTS = 0）：事务已回滚，忽略锁
```

**CheckTxnStatus 的作用**：
- 查询事务的当前状态
- 如果事务超时，可以主动清理锁
- 返回 TTL、commitTS、action 等信息

## 二、实现的核心功能

### 2.1 构建 Prewrite 请求 (2pc.go)

```go
func (c *twoPhaseCommitter) buildPrewriteRequest(batch batchKeys) *tikvrpc.Request {
    mutations := make([]*pb.Mutation, 0, len(batch.keys))
    for _, key := range batch.keys {
        mutation := c.mutations[string(key)]
        mutations = append(mutations, &mutation.Mutation)
    }
    
    req := &pb.PrewriteRequest{
        Mutations:    mutations,      // 要写入的数据
        PrimaryLock:  c.primary(),    // Primary key
        StartVersion: c.startTS,      // 事务开始时间戳
        LockTtl:      c.lockTTL,      // 锁的过期时间
    }
    return tikvrpc.NewRequest(tikvrpc.CmdPrewrite, req, pb.Context{})
}
```

**关键点**：
- `Mutations` 包含所有要写入的 key-value 对
- `PrimaryLock` 指向事务的 Primary key
- `StartVersion` 作为 MVCC 的版本号
- `LockTtl` 防止事务崩溃后锁永久存在

### 2.2 处理 Commit 响应 (2pc.go)

```go
commitResp := resp.Resp.(*pb.CommitResponse)
if commitResp.Error != nil {
    // 使用 extractKeyErr 转换错误类型
    err := extractKeyErr(commitResp.Error)
    return errors.Trace(err)
}
```

**为什么要用 extractKeyErr？**
- TiKV 返回的 `KeyError` 包含多种错误类型
- `Conflict`：写写冲突，需要重试
- `Retryable`：可重试错误
- 直接用 `errors.Errorf` 会丢失错误类型信息

### 2.3 事务状态查询 (lock_resolver.go)

```go
req := tikvrpc.NewRequest(tikvrpc.CmdCheckTxnStatus, &kvrpcpb.CheckTxnStatusRequest{
    PrimaryKey: primary,
    LockTs:     txnID,
    CurrentTs:  currentTS,
}, kvrpcpb.Context{})
```

**CheckTxnStatus 的返回值**：
```go
status.action = cmdResp.Action        // NoAction/Rollback/LockNotExistRollback
status.commitTS = cmdResp.CommitVersion  // 提交时间戳（0 表示未提交）
status.ttl = cmdResp.LockTtl          // 锁的剩余 TTL
```

### 2.4 按 Region 分组 (region_cache.go)

```go
func (c *RegionCache) GroupKeysByRegion(bo *Backoffer, keys [][]byte, 
    filter func(key, regionStartKey []byte) bool) (map[RegionVerID][][]byte, RegionVerID, error) {
    
    groups := make(map[RegionVerID][][]byte)
    var firstRegion RegionVerID
    
    for _, key := range keys {
        loc, err := c.LocateKey(bo, key)
        if err != nil {
            return nil, RegionVerID{}, err
        }
        
        if filter != nil && filter(key, loc.StartKey) {
            continue
        }
        
        if !firstRegionSet {
            firstRegion = loc.Region  // Primary key 所在的 Region
            firstRegionSet = true
        }
        
        groups[loc.Region] = append(groups[loc.Region], key)
    }
    
    return groups, firstRegion, nil
}
```

**为什么要分组？**
- TiKV 是分布式存储，数据分散在多个 Region
- 同一个 Region 的 keys 可以批量发送，减少 RPC 次数
- `firstRegion` 用于确定 Primary key 的位置


## 三、遇到的问题与解决过程

### 3.1 问题一：TestCheckTxnStatusTTL 失败

**现象**：
```
Expected: 0x0
Actual:   0x3e8 (1000)
```

测试期望事务回滚后 TTL 为 0，但实际返回 1000。

**问题分析**：

1. **测试逻辑**：
```go
// 1. 开启事务，写入 key
txn.Set(key, value)
txn.Rollback()

// 2. 查询事务状态
status := getTxnStatus(txn.StartTS(), key)
// 期望：status.ttl == 0（已回滚）
```

2. **代码追踪**：
```go
// lock_resolver.go 原始代码
if status.IsCommitted() || status.action == kvrpcpb.Action_NoAction {
    lr.saveResolved(txnID, status)  // 缓存状态
}
```

3. **根本原因**：
- `Action_NoAction` 有两种含义：
  - 活跃锁（TTL > 0）：事务正在进行
  - 已回滚（TTL = 0）：事务已结束
- 原代码把活跃锁也缓存了，导致后续查询返回过期数据

**解决方案**：

只缓存终态（TTL = 0）：
```go
// 修改后的代码
if status.ttl == 0 && (status.IsCommitted() || status.action == kvrpcpb.Action_NoAction) {
    lr.saveResolved(txnID, status)
}
```

**思考过程**：
1. 为什么会缓存活跃锁？因为没有区分 `Action_NoAction` 的两种情况
2. 如何判断是否应该缓存？只有终态（TTL=0）才是稳定的，可以缓存
3. 是否会影响性能？不会，因为活跃锁本来就不应该缓存

### 3.2 问题二：TestWriteWriteConflict 失败

**现象**：
```
Error Matches: .*write conflict.*
Actual error: commit failed: ...
```

测试期望捕获到 `write conflict` 错误，但实际错误类型不匹配。

**问题分析**：

1. **测试逻辑**：
```go
// 两个事务同时写同一个 key
txn1.Set(key, value1)
txn2.Set(key, value2)

txn1.Commit()  // 成功
txn2.Commit()  // 应该返回 write conflict 错误
```

2. **代码追踪**：
```go
// 2pc.go 原始代码
commitResp := resp.Resp.(*pb.CommitResponse)
if commitResp.Error != nil {
    return errors.Errorf("commit failed: %v", commitResp.Error)
}
```

3. **根本原因**：
- TiKV 返回的 `KeyError` 包含 `Conflict` 字段
- 使用 `errors.Errorf` 创建的是普通错误，丢失了类型信息
- 测试代码通过 `kv.IsRetryableError` 判断错误类型，无法识别

**解决方案**：

使用 `extractKeyErr` 转换错误类型：
```go
// 修改后的代码
commitResp := resp.Resp.(*pb.CommitResponse)
if commitResp.Error != nil {
    err := extractKeyErr(commitResp.Error)  // 转换为 kv.ErrWriteConflict
    return errors.Trace(err)
}
```

**extractKeyErr 的实现**：
```go
func extractKeyErr(keyErr *pb.KeyError) error {
    if keyErr.Conflict != nil {
        return newWriteConflictError(keyErr.Conflict)  // 返回 kv.ErrWriteConflict
    }
    if keyErr.Retryable != "" {
        return kv.ErrTxnRetryable.GenWithStackByArgs(...)
    }
    // ...
}
```

**思考过程**：
1. 为什么 Prewrite 阶段没问题？因为 Prewrite 已经用了 `extractKeyErr`
2. 为什么要保留错误类型？因���上层需要根据错误类型决定是否重试
3. 是否需要修改测试？不需要，测试是对的，代码实现有问题

### 3.3 问题三：Failpoint 依赖

**现象**：
- `make proj6` 全部通过
- `go test -v` 出现 6 个新的失败

**问题分析**：

1. **Makefile 中的 proj6 目标**：
```makefile
proj6:
	@make failpoint-enable
	@go test -v ./store/tikv/... -run TestCheckTxnStatusTTL|TestWriteWriteConflict
	@make failpoint-disable
```

2. **Failpoint 的作用**：
- 在代码中注入故障点，模拟各种异常情况
- 例如：网络超时、Region 迁移、锁冲突等
- 测试代码依赖这些故障点来验证错误处理逻辑

3. **为什么直接运行 go test 会失败**：
- Failpoint 没有启用，某些测试场景无法触发
- 代码中的 `failpoint.Inject` 不会执行

**解决方案**：

始终使用 `make proj6` 运行测试，确保 failpoint 正确启用。

**延伸思考**：
- Failpoint 是一种测试技术，类似于依赖注入
- 生产环境中 failpoint 代码会被优化掉，不影响性能
- 这种测试方法在分布式系统中非常重要


## 四、技术要点深入

### 4.1 MVCC 与时间戳

**多版本并发控制（MVCC）**：
```
Key: user_1
Versions:
  - TS=100: {name: "Alice", age: 20}
  - TS=200: {name: "Alice", age: 21}
  - TS=300: {name: "Alice", age: 22}
```

**时间戳的作用**：
- `StartTS`：事务开始时间，作为读取的快照版本
- `CommitTS`：事务提交时间，作为写入的版本号
- 读取时只能看到 `TS <= StartTS` 的版本

**为什么需要两个时间戳？**
- `StartTS` 确定读取的一致性快照
- `CommitTS` 确定写入的顺序
- 两者分离实现了快照隔离（Snapshot Isolation）

### 4.2 乐观锁 vs 悲观锁

**乐观锁（Percolator 使用）**：
```go
// 1. 读取数据（不加锁）
value := txn.Get(key)

// 2. 修改数据
newValue := value + 1

// 3. 提交时检测冲突
txn.Set(key, newValue)
err := txn.Commit()  // 可能失败，需要重试
```

**优点**：
- 读操作不阻塞
- 适合冲突少的场景
- 吞吐量高

**缺点**：
- 冲突时需要重试
- 不适合高冲突场景

**悲观锁**：
```go
// 1. 加锁
txn.Lock(key)

// 2. 读取和修改
value := txn.Get(key)
txn.Set(key, value + 1)

// 3. 提交（一定成功）
txn.Commit()
```

**TiDB 的选择**：
- 默认使用乐观锁
- 提供悲观锁选项（`BEGIN PESSIMISTIC`）
- 根据业务场景选择

### 4.3 Primary Key 的设计

**为什么需要 Primary Key？**

在分布式事务中，如果没有 Primary Key：
```
Scenario: 事务写入 3 个 key，分布在 3 个 Region

Region1: key1 (Prewrite 成功)
Region2: key2 (Prewrite 成功)
Region3: key3 (Prewrite 失败)

问题：如何通知 Region1 和 Region2 回滚？
```

**有了 Primary Key**：
```
Region1: key1 (Primary) - 决策点
Region2: key2 (Secondary, 指向 key1)
Region3: key3 (Secondary, 指向 key1)

流程：
1. Prewrite 所有 keys
2. Commit Primary (key1)
   - 成功：事务成功，异步 Commit Secondary keys
   - 失败：事务失败，清理所有锁
3. Secondary keys 通过 Primary 的状态判断事务结果
```

**Primary Key 的选择策略**：
- 通常选择第一个写入的 key
- 可以选择访问频率最高的 key
- 避免选择热点 key

### 4.4 锁的生命周期

```
1. 创建锁（Prewrite）
   Lock {
       primary: key1,
       startTS: 100,
       ttl: 3000,  // 3 秒
   }

2. 锁的状态变化
   - 活跃：TTL > 0，事务正在进行
   - 超时：TTL 过期，可以被清理
   - 提交：写入 Write 列，删除 Lock
   - 回滚：直接删除 Lock

3. 锁的清理
   - 正常：事务 Commit/Rollback 时清理
   - 异常：其他事务通过 CheckTxnStatus 清理
```

**TTL 的计算**：
```go
// 根据事务大小动态调整
ttl = min(maxTTL, txnSize * ttlFactor)
```

### 4.5 Region 与分布式事务

**Region 的概念**：
```
TiKV 集群
├── Region1: [a, m)
├── Region2: [m, z)
└── Region3: [z, ∞)
```

**跨 Region 事务**：
```go
// 事务写入 5 个 keys
keys := []string{"apple", "banana", "mango", "orange", "zebra"}

// 按 Region 分组
groups := {
    Region1: ["apple", "banana"],
    Region2: ["mango", "orange"],
    Region3: ["zebra"],
}

// 并行发送 Prewrite 请求
for region, keys := range groups {
    go prewrite(region, keys)
}
```

**Region 分裂与合并**：
- Region 过大时自动分裂
- Region 过小时自动合并
- 事务需要处理 Region 变化（通过 RegionCache）


## 五、延伸知识

### 5.1 与其他分布式事务协议的对比

#### Percolator vs 2PC (Two-Phase Commit)

**传统 2PC**：
```
Coordinator
    ↓
Prepare → All Participants
    ↓
Commit/Abort → All Participants
```

**问题**：
- Coordinator 是单点故障
- 阻塞：Prepare 阶段所有参与者都要等待

**Percolator 的改进**：
- 无中心化 Coordinator
- Primary Key 作为决策点
- 非阻塞：Secondary keys 可以异步提交

#### Percolator vs Spanner

**Spanner（Google）**：
- 使用 TrueTime API（原子钟 + GPS）
- 提供外部一致性（External Consistency）
- 支持跨数据中心事务

**Percolator**：
- 使用逻辑时钟（TSO）
- 提供快照隔离（Snapshot Isolation）
- 更轻量，更容易实现

### 5.2 TiDB 的事务隔离级别

**支持的隔离级别**：
```sql
-- 快照隔离（默认）
SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ;

-- 读已提交
SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;
```

**快照隔离 vs 可串行化**：

快照隔离允许写偏斜（Write Skew）：
```sql
-- 初始状态：account1 = 100, account2 = 100
-- 约束：account1 + account2 >= 100

-- Txn1
BEGIN;
SELECT account1, account2;  -- 100, 100
UPDATE account1 SET balance = 0;  -- 满足约束
COMMIT;

-- Txn2（并发）
BEGIN;
SELECT account1, account2;  -- 100, 100
UPDATE account2 SET balance = 0;  -- 满足约束
COMMIT;

-- 结果：account1 = 0, account2 = 0（违反约束！）
```

**TiDB 的解决方案**：
- 使用悲观锁避免写偏斜
- 应用层检查约束

### 5.3 性能优化技巧

#### 1. 减少跨 Region 事务

**不好的设计**：
```go
// 用户 ID 作为 key
key := fmt.Sprintf("user_%d", userID)
```

**问题**：用户分布在所有 Region，事务经常跨 Region

**优化**：
```go
// 按业务分区
key := fmt.Sprintf("order_%s_%d", region, orderID)
```

#### 2. 批量操作

**不好的做法**：
```go
for _, key := range keys {
    txn.Set(key, value)
    txn.Commit()  // 每次都提交
}
```

**优化**：
```go
txn := db.Begin()
for _, key := range keys {
    txn.Set(key, value)
}
txn.Commit()  // 一次提交
```

#### 3. 避免大事务

**问题**：
- 大事务占用内存多
- 冲突概率高
- 提交时间长

**建议**：
- 单个事务 < 100MB
- 写入 keys < 10000
- 超过限制时拆分事务

### 5.4 故障场景与恢复

#### 场景 1：Client 崩溃

```
1. Client 发送 Prewrite 请求
2. Prewrite 成功，写入锁
3. Client 崩溃，未发送 Commit

结果：锁永久存在？
```

**解决**：
- 锁有 TTL，超时后可以被清理
- 其他事务通过 CheckTxnStatus 清理过期锁

#### 场景 2：TiKV 节点崩溃

```
1. Prewrite 成功
2. Commit Primary 成功
3. TiKV 节点崩溃，Secondary keys 未提交

结果：事务状态不一致？
```

**解决**：
- Primary 已提交，事务算成功
- 读取 Secondary keys 时，发现锁指向已提交的 Primary
- 自动完成 Commit（异步提交）

#### 场景 3：网络分区

```
Region1 和 Region2 网络分区

Txn1: 写入 Region1 和 Region2
```

**解决**：
- Prewrite 阶段会超时失败
- 事务回滚，保证一致性
- 网络恢复后重试

### 5.5 实际应用场景

#### 1. 银行转账

```go
func Transfer(from, to string, amount int) error {
    txn := db.Begin()
    defer txn.Rollback()
    
    // 读取余额
    fromBalance := txn.Get(from)
    toBalance := txn.Get(to)
    
    // 检查余额
    if fromBalance < amount {
        return errors.New("insufficient balance")
    }
    
    // 转账
    txn.Set(from, fromBalance - amount)
    txn.Set(to, toBalance + amount)
    
    return txn.Commit()
}
```

**关键点**：
- 使用事务保证原子性
- 快照隔离避免脏读
- 冲突时自动重试

#### 2. 库存扣减

```go
func DeductInventory(productID string, quantity int) error {
    for retry := 0; retry < 3; retry++ {
        txn := db.Begin()
        
        inventory := txn.Get(productID)
        if inventory < quantity {
            return errors.New("out of stock")
        }
        
        txn.Set(productID, inventory - quantity)
        
        err := txn.Commit()
        if err == nil {
            return nil
        }
        
        // 写冲突，重试
        if kv.IsRetryableError(err) {
            continue
        }
        return err
    }
    return errors.New("too many retries")
}
```

**优化**：
- 高并发场景使用悲观锁
- 或者使用分布式锁（Redis）

#### 3. 订单系统

```go
func CreateOrder(order Order) error {
    txn := db.Begin()
    defer txn.Rollback()
    
    // 1. 扣减库存
    for _, item := range order.Items {
        inventory := txn.Get(item.ProductID)
        if inventory < item.Quantity {
            return errors.New("out of stock")
        }
        txn.Set(item.ProductID, inventory - item.Quantity)
    }
    
    // 2. 创建订单
    txn.Set(order.ID, order)
    
    // 3. 扣减余额
    balance := txn.Get(order.UserID)
    if balance < order.Amount {
        return errors.New("insufficient balance")
    }
    txn.Set(order.UserID, balance - order.Amount)
    
    return txn.Commit()
}
```

**特点**：
- 多个操作在一个事务中
- 保证数据一致性
- 失败时自动回滚


## 六、调试技巧与工具

### 6.1 日志分析

**关键日志点**：
```go
// 在关键位置添加日志
logutil.BgLogger().Info("prewrite",
    zap.Uint64("startTS", c.startTS),
    zap.Int("keyCount", len(keys)),
    zap.String("primary", string(c.primary())))
```

**日志级别**：
- `Debug`：详细的执行流程
- `Info`：关键操作
- `Warn`：异常但可恢复
- `Error`：严重错误

### 6.2 Failpoint 使用

**注入故障点**：
```go
// 在代码中添加
failpoint.Inject("prewriteFail", func() {
    return errors.New("injected error")
})
```

**测试中启用**：
```go
// 启用 failpoint
failpoint.Enable("tikvclient/prewriteFail", "return(true)")

// 运行测试
txn.Commit()  // 会触发注入的错误

// 禁用 failpoint
failpoint.Disable("tikvclient/prewriteFail")
```

### 6.3 性能分析

**使用 pprof**：
```bash
# 运行测试并生成 profile
go test -cpuprofile=cpu.prof -memprofile=mem.prof

# 分析 CPU 使用
go tool pprof cpu.prof

# 分析内存使用
go tool pprof mem.prof
```

**常见性能瓶颈**：
- 频繁的 RPC 调用
- 大量的内存分配
- 锁竞争

### 6.4 测试策略

**单元测试**：
```go
func TestBuildPrewriteRequest(t *testing.T) {
    committer := &twoPhaseCommitter{
        startTS: 100,
        lockTTL: 3000,
        mutations: map[string]*mutationEx{
            "key1": {Mutation: pb.Mutation{Op: pb.Op_Put, Key: []byte("key1"), Value: []byte("value1")}},
        },
    }
    
    req := committer.buildPrewriteRequest(batchKeys{keys: [][]byte{[]byte("key1")}})
    
    assert.NotNil(t, req)
    assert.Equal(t, tikvrpc.CmdPrewrite, req.Type)
}
```

**集成测试**：
```go
func TestWriteWriteConflict(t *testing.T) {
    store := NewTestStore()
    
    txn1 := store.Begin()
    txn2 := store.Begin()
    
    txn1.Set([]byte("key"), []byte("value1"))
    txn2.Set([]byte("key"), []byte("value2"))
    
    err1 := txn1.Commit()
    assert.NoError(t, err1)
    
    err2 := txn2.Commit()
    assert.True(t, kv.IsRetryableError(err2))
}
```

## 七、总结与反思

### 7.1 核心收获

通过 Project 6，我掌握了：

1. **分布式事务的实现原理**
   - 两阶段提交协议
   - MVCC 与时间戳
   - 乐观锁机制

2. **工程实践能力**
   - 阅读和理解大型代码库
   - 调试分布式系统
   - 编写健壮的错误处理代码

3. **问题解决思路**
   - 从测试失败反推问题根因
   - 分析代码逻辑找出 bug
   - 验证修复方案的正确性

### 7.2 关键经验

1. **理解业务逻辑比写代码更重要**
   - 先理解 Percolator 协议
   - 再看代码实现
   - 最后动手编码

2. **测试是最好的文档**
   - 测试用例展示了预期行为
   - 失败的测试指出了问题所在
   - 通过的测试验证了正确性

3. **错误处理不是可选项**
   - 分布式系统中错误是常态
   - 正确的错误类型至关重要
   - 重试机制需要仔细设计

### 7.3 后续学习方向

1. **深入 TiKV 源码**
   - Raft 共识算法
   - RocksDB 存储引擎
   - Coprocessor 计算下推

2. **分布式系统理论**
   - CAP 定理
   - 一致性模型
   - 分布式共识

3. **性能优化**
   - 批量操作
   - 并发控制
   - 缓存策略

4. **生产环境实践**
   - 监控和告警
   - 故障排查
   - 容量规划

### 7.4 实用建议

1. **遇到问题时**
   - 先看测试用例，理解预期行为
   - 添加日志，追踪执行流程
   - 使用 debugger 单步调试

2. **写代码时**
   - 参考已有的实现（如 Prewrite）
   - 保持代码风格一致
   - 添加必要的注释

3. **提交代码前**
   - 运行所有测试
   - 检查代码格式
   - 确认没有遗留的 debug 代码

## 八、参考资料

1. **论文**
   - [Large-scale Incremental Processing Using Distributed Transactions and Notifications](https://research.google/pubs/pub36726/)（Percolator 原始论文）

2. **文档**
   - [TiKV 事务模型](https://tikv.org/deep-dive/distributed-transaction/introduction/)
   - [TiDB 事务隔离级别](https://docs.pingcap.com/zh/tidb/stable/transaction-isolation-levels)

3. **代码**
   - [TiKV 源码](https://github.com/tikv/tikv)
   - [TiDB 源码](https://github.com/pingcap/tidb)

4. **博客**
   - [TiDB 的 MVCC 实现](https://pingcap.com/blog-cn/tidb-mvcc/)
   - [分布式事务的实现](https://pingcap.com/blog-cn/distributed-transaction/)

---

**最后的话**：

分布式事务是一个复杂的话题，Percolator 提供了一个优雅的解决方案。通过这个项目，我不仅学会了如何实现分布式事务，更重要的是理解了背后的设计思想和权衡。

在实际工作中，我们可能不会从头实现一个分布式事务系统，但理解其原理对于：
- 正确使用数据库事务
- 设计高并发系统
- 排查生产环境问题

都有很大帮助。

继续加油！🚀

