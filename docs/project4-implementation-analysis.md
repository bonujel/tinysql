# TinySQL Project 4 查询优化器实现分析

## 一、项目背景

TinySQL Project 4 聚焦于数据库查询优化器的实现，基于经典的 System R 优化器框架。查询优化器是数据库系统的核心组件，负责将逻辑查询计划转换为高效的物理执行计划。

### 1.1 优化器在 SQL 执行流程中的位置

```
SQL 文本 → Parser → AST → Validator → Type Infer
    → Logical Optimizer → Physical Optimizer → Executor
```

优化器分为两个阶段：
- **逻辑优化**：基于规则的优化，如谓词下推、列裁剪等
- **物理优化**：基于代价的优化，选择最优的物理执行计划

### 1.2 System R 优化器框架

System R 是现代数据库基于代价优化（CBO）的鼻祖。TinySQL 采用了这一经典框架，通过两阶段优化来生成高效的执行计划。

## 二、核心实现内容

Project 4 分为两个部分，共实现了四个核心功能：

### Part 1: 谓词下推（Predicate Push Down）
### Part 2:
- Count-Min Sketch 统计信息
- Join Reorder 动态规划算法
- Skyline Pruning 访问路径剪枝

## 三、技术实现详解

### 3.1 谓词下推（Predicate Push Down）

#### 实现思路

谓词下推是一种经典的逻辑优化规则，核心思想是将过滤条件尽可能下推到数据源附近，减少中间结果集的大小。

**基本原理**：
- 自顶向下遍历逻辑计划树
- 每个节点判断哪些谓词可以下推给子节点
- 不能下推的谓词保留在当前节点

#### 关键代码实现

位置：`planner/core/rule_predicate_push_down.go`

```go
func (la *LogicalAggregation) PredicatePushDown(predicates []expression.Expression)
    (ret []expression.Expression, retPlan LogicalPlan) {

    canBePushed := make([]expression.Expression, 0)
    canNotBePushed := make([]expression.Expression, 0)

    childSchema := la.children[0].Schema()

    for _, cond := range predicates {
        switch cond.(type) {
        case *expression.Constant:
            // 常量谓词可以下推
            canBePushed = append(canBePushed, cond)
        case *expression.ScalarFunction:
            // 检查谓词引用的列是否都在子节点 Schema 中
            if expression.ExprFromSchema(cond, childSchema) {
                canBePushed = append(canBePushed, cond)
            } else {
                canNotBePushed = append(canNotBePushed, cond)
            }
        }
    }

    remained, child := la.children[0].PredicatePushDown(canBePushed)
    return append(remained, canNotBePushed...), la
}
```

#### 实现要点

1. **聚合算子的谓词下推**：
   - 只有引用 GROUP BY 列的谓词才能下推
   - 引用聚合函数结果的谓词不能下推
   - 常量谓词总是可以下推

2. **Join 算子的谓词下推**：
   - Inner Join：所有谓词都可以尝试下推
   - Left/Right Outer Join：需要区分左右表，只能下推特定谓词
   - 需要处理等值条件和其他条件

3. **Projection 算子的谓词下推**：
   - 需要进行列替换（Column Substitution）
   - 将谓词中的列替换为 Projection 的表达式

### 3.2 Count-Min Sketch 统计信息

#### 实现思路

Count-Min Sketch 是一种概率数据结构，用于估计数据流中元素的频率。它使用多个哈希函数和计数器数组来存储频率信息。

**核心特点**：
- 空间效率高，适合大数据场景
- 查询时间复杂度 O(d)，d 为深度
- 存在过估计，但不会低估

#### 关键代码实现

位置：`statistics/cmsketch.go`

```go
// 插入操作
func (c *CMSketch) insertBytesByCount(bytes []byte, count uint64) {
    h1, h2 := murmur3.Sum128(bytes)
    c.count += count
    for i := range c.table {
        j := (h1 + h2*uint64(i)) % uint64(c.width)
        c.table[i][j] += uint32(count)
    }
}

// 查询操作
func (c *CMSketch) queryHashValue(h1, h2 uint64) uint64 {
    vals := make([]uint32, c.depth)
    min := uint32(^uint32(0))

    for i := range c.table {
        j := (h1 + h2*uint64(i)) % uint64(c.width)
        if min > c.table[i][j] {
            min = c.table[i][j]
        }
        // 噪声消除
        noise := (c.count - uint64(c.table[i][j])) / (uint64(c.width) - 1)
        if uint64(c.table[i][j]) < noise {
            vals[i] = 0
        } else {
            vals[i] = c.table[i][j] - uint32(noise)
        }
    }

    // 排序并取中位数
    sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
    res := vals[(c.depth-1)/2] + (vals[c.depth/2]-vals[(c.depth-1)/2])/2

    if res > min {
        res = min
    }
    return uint64(res)
}
```

#### 实现要点

1. **哈希函数选择**：
   - 使用 MurmurHash3 生成两个独立的哈希值
   - 通过线性组合生成多个哈希函数：`h(i) = h1 + h2 * i`

2. **噪声消除**：
   - 计算每个位置的噪声估计值
   - 从计数器值中减去噪声
   - 避免过度估计

3. **中位数估计**：
   - 对所有行的估计值排序
   - 取中位数作为最终结果
   - 同时取最小值作为上界

### 3.3 Join Reorder 动态规划算法

#### 实现思路

Join Reorder 是物理优化的关键步骤，目标是找到代价最小的 Join 顺序。使用动态规划算法可以在多项式时间内找到最优解。

**算法核心**：
- 使用二进制位表示节点集合
- 状态：`dp[mask]` 表示包含 mask 中节点的最优 Join 树
- 转移：枚举 mask 的所有子集划分

#### 关键代码实现

位置：`planner/core/rule_join_reorder_dp.go`

```go
func (s *joinReorderDPSolver) solve(joinGroup []LogicalPlan,
    eqConds []expression.Expression) (LogicalPlan, error) {

    n := len(joinGroup)
    dpTable := make(map[uint]LogicalPlan)
    costTable := make(map[uint]float64)

    // 初始化单节点状态
    for i := 0; i < n; i++ {
        mask := uint(1 << uint(i))
        dpTable[mask] = joinGroup[i]
        costTable[mask] = s.baseNodeCumCost(joinGroup[i])
    }

    // DP 主循环
    for size := 2; size <= n; size++ {
        for group := uint(0); group < (1 << uint(n)); group++ {
            if countOnes(group) != size {
                continue
            }

            var bestPlan LogicalPlan
            var minCost float64 = -1

            // 使用 Gosper's Hack 枚举子集
            for sub := (group - 1) & group; sub > 0; sub = (sub - 1) & group {
                complement := group ^ sub

                leftPlan, leftOK := dpTable[sub]
                rightPlan, rightOK := dpTable[complement]
                if !leftOK || !rightOK {
                    continue
                }

                // 检查连接边
                edges := s.findUsableEdges(sub, complement, eqEdgeMap)
                if len(edges) == 0 {
                    continue
                }

                // 尝试两种连接顺序
                newJoin1, _ := s.newJoinWithEdge(leftPlan, rightPlan, edges, otherConds)
                totalCost1 := costTable[sub] + costTable[complement] +
                             newJoin1.statsInfo().RowCount

                if minCost < 0 || totalCost1 < minCost {
                    minCost = totalCost1
                    bestPlan = newJoin1
                }
            }

            if bestPlan != nil {
                dpTable[group] = bestPlan
                costTable[group] = minCost
            }
        }
    }

    return dpTable[uint((1<<uint(n))-1)], nil
}
```

#### 实现要点

1. **状态压缩**：
   - 使用 uint 的二进制位表示节点集合
   - 例如：`1011` 表示包含节点 0, 1, 3
   - 最多支持 32 个表

2. **子集枚举**：
   - 使用 Gosper's Hack 高效枚举子集
   - `sub = (sub - 1) & group` 生成下一个子集
   - 避免枚举所有可能的组合

3. **代价计算**：
   - 累积代价 = 左子树代价 + 右子树代价 + Join 代价
   - Join 代价用输出行数估计
   - 选择代价最小的方案

4. **Tie-breaking 策略**：
   - 当代价相同时，优先选择左子集更小的方案
   - 倾向于生成右深树
   - 保证结果的稳定性

5. **笛卡尔积处理**：
   - 使用 BFS 找到所有连通分量
   - 对断开的连接图使用 bushy join 合并
   - 避免生成过大的笛卡尔积

### 3.4 Skyline Pruning 访问路径剪枝

#### 实现思路

Skyline Pruning 是一种启发式剪枝策略，用于在多个索引中快速筛除明显劣势的选项。它基于 Skyline 算子的思想，在多个维度上比较候选路径。

**核心思想**：
- 如果路径 A 在所有维度上都不比路径 B 差，且至少有一个维度更好，则 A 支配 B
- 被支配的路径可以安全剪除
- 保留 Skyline 集合中的路径进行代价计算

#### 关键代码实现

位置：`planner/core/find_best_task.go`

```go
func compareCandidates(lhs, rhs *candidatePath) int {
    // 维度 1: 比较列集合（越多越好）
    lhsSubsetRhs := lhs.columnSet.SubsetOf(rhs.columnSet)
    rhsSubsetLhs := rhs.columnSet.SubsetOf(lhs.columnSet)

    var colCmp int
    if rhsSubsetLhs && !lhsSubsetRhs {
        colCmp = 1  // lhs 包含更多列
    } else if lhsSubsetRhs && !rhsSubsetLhs {
        colCmp = -1 // rhs 包含更多列
    } else {
        colCmp = 0  // 不可比或相等
    }

    // 维度 2: 比较物理属性匹配
    var propCmp int
    if lhs.isMatchProp && !rhs.isMatchProp {
        propCmp = 1
    } else if !lhs.isMatchProp && rhs.isMatchProp {
        propCmp = -1
    } else {
        propCmp = 0
    }

    // 维度 3: 比较是否需要双扫描
    var scanCmp int
    if lhs.isSingleScan && !rhs.isSingleScan {
        scanCmp = 1  // 单次扫描更好
    } else if !lhs.isSingleScan && rhs.isSingleScan {
        scanCmp = -1
    } else {
        scanCmp = 0
    }

    // Skyline 判断
    if colCmp >= 0 && propCmp >= 0 && scanCmp >= 0 {
        if colCmp > 0 || propCmp > 0 || scanCmp > 0 {
            return 1  // lhs 支配 rhs
        }
    }

    if colCmp <= 0 && propCmp <= 0 && scanCmp <= 0 {
        if colCmp < 0 || propCmp < 0 || scanCmp < 0 {
            return -1 // rhs 支配 lhs
        }
    }

    return 0 // 不可比或完全相等
}
```

#### 实现要点

1. **三个比较维度**：
   - **列集合**：访问条件涉及的列越多越好
   - **属性匹配**：匹配物理属性（如排序）可以避免额外排序
   - **扫描方式**：单次扫描优于双扫描（回表）

2. **支配关系判断**：
   - A 支配 B：A 在所有维度 ≥ B，且至少一个维度 > B
   - 使用三个比较值的组合判断支配关系
   - 返回值：1（A 支配 B）、-1（B 支配 A）、0（不可比）

3. **列集合比较**：
   - 使用 `SubsetOf` 方法检查包含关系
   - 超集优于子集（过滤更多数据）
   - 不可比的集合保留

4. **剪枝效果**：
   - 在代价计算前筛除劣势路径
   - 减少搜索空间
   - 保证不会错过最优解

## 四、实现过程中的问题与解决

### 4.1 Join Reorder 的边界情况

**问题**：
- 初始实现未考虑断开的连接图（笛卡尔积）
- 测试用例 `TestDPReorderAllCartesian` 失败

**解决方案**：
1. 使用 BFS 找到所有连通分量
2. 对每个连通分量独立进行 DP
3. 使用 bushy join 合并连通分量
4. 处理涉及多表的 other conditions

### 4.2 Count-Min Sketch 的精度问题

**问题**：
- 直接取最小值会导致过度估计
- 噪声影响查询准确性

**解决方案**：
1. 实现噪声消除算法
2. 计算每个位置的噪声估计值：`noise = (total - count) / (width - 1)`
3. 从计数器值中减去噪声
4. 使用中位数估计提高鲁棒性
5. 同时取最小值作为上界

### 4.3 Skyline Pruning 的比较逻辑

**问题**：
- 初始实现的支配关系判断不完整
- 某些不可比的路径被错误剪除

**解决方案**：
1. 明确三个维度的比较规则
2. 正确实现 Skyline 判断逻辑
3. 确保只有被严格支配的路径才被剪除
4. 处理完全相等的情况

### 4.4 谓词下推的列替换

**问题**：
- Projection 算子的谓词下推需要列替换
- 初始实现未正确处理表达式替换

**解决方案**：
1. 使用 `expression.ColumnSubstitute` 进行列替换
2. 将谓词中的列替换为 Projection 的表达式
3. 检查替换后的表达式是否包含 GetSetVarFunc
4. 不能下推包含变量赋值的表达式

## 五、测试与验证

### 5.1 测试用例覆盖

所有测试用例均通过：

1. **TestPredicatePushDown**：
   - 测试各种算子的谓词下推
   - 验证 Join、Aggregation、Projection 的正确性

2. **TestCMSketch**：
   - 测试插入和查询操作
   - 验证频率估计的准确性
   - 测试合并操作

3. **TestDPReorderTPCHQ5**：
   - 测试 TPC-H Q5 的 Join 重排序
   - 验证复杂查询的优化效果

4. **TestDPReorderAllCartesian**：
   - 测试笛卡尔积的处理
   - 验证断开连接图的正确性

5. **TestSkylinePruning**：
   - 测试访问路径剪枝
   - 验证支配关系判断

### 5.2 性能验证

通过 `make test-proj4-1` 和 `make test-proj4-2` 运行所有测试：

```bash
$ make test-proj4-1
PASS: TestPredicatePushDown

$ make test-proj4-2
PASS: TestCMSketch
PASS: TestDPReorderTPCHQ5
PASS: TestDPReorderAllCartesian
PASS: TestSkylinePruning
```

## 六、技术亮点

### 6.1 算法优化

1. **Gosper's Hack 子集枚举**：
   - 高效枚举二进制子集
   - 避免不必要的组合
   - 时间复杂度 O(3^n)

2. **噪声消除算法**：
   - 提高 Count-Min Sketch 精度
   - 减少过估计
   - 使用中位数提高鲁棒性

3. **Tie-breaking 策略**：
   - 保证结果稳定性
   - 倾向于生成右深树
   - 优化执行效率

### 6.2 代码质量

1. **清晰的注释**：
   - 关键算法步骤都有中文注释
   - 解释设计决策和边界情况

2. **模块化设计**：
   - 每个功能独立实现
   - 接口清晰，易于测试

3. **错误处理**：
   - 完善的错误检查
   - 合理的边界条件处理

## 七、总结与展望

### 7.1 实现总结

Project 4 成功实现了 TinySQL 查询优化器的核心功能：

1. **逻辑优化**：谓词下推规则
2. **统计信息**：Count-Min Sketch 频率估计
3. **物理优化**：Join Reorder 和 Skyline Pruning

这些功能构成了一个完整的查询优化器框架，能够生成高效的执行计划。

### 7.2 技术收获

1. **深入理解优化器原理**：
   - System R 两阶段优化框架
   - 基于规则和基于代价的优化
   - 动态规划在查询优化中的应用

2. **掌握经典算法**：
   - Count-Min Sketch 概率数据结构
   - Skyline 算子和支配关系
   - 状态压缩动态规划

3. **工程实践能力**：
   - 大型代码库的理解和修改
   - 测试驱动开发
   - 性能优化和调试

### 7.3 可能的改进方向

1. **更多优化规则**：
   - 列裁剪（Column Pruning）
   - 聚合下推（Aggregation Push Down）
   - TopN 下推

2. **更精确的代价模型**：
   - 考虑 CPU 代价
   - 考虑 I/O 代价
   - 考虑网络代价

3. **更高级的统计信息**：
   - 直方图（Histogram）
   - 多列统计信息
   - 自适应统计信息更新

4. **Cascades 优化器**：
   - 探索 Cascades 框架
   - 实现 transformation rules
   - 对比两种框架的优劣

## 八、参考资料

1. [TiDB 源码阅读系列文章（七）基于规则的优化](https://pingcap.com/zh/blog/tidb-source-code-reading-7)
2. [TiDB 源码阅读系列文章（八）基于代价的优化](https://pingcap.com/zh/blog/tidb-source-code-reading-8)
3. [TiDB 源码阅读系列文章（十二）统计信息（上）](https://pingcap.com/blog-cn/tidb-source-code-reading-12/)
4. [TiDB Cascades Planner 原理解析](https://pingcap.com/blog-cn/tidb-cascades-planner/)
5. [Skyline Pruning Proposal](https://github.com/pingcap/tidb/blob/master/docs/design/2019-01-25-skyline-pruning.md)
6. [Count-Min Sketch Wikipedia](https://en.wikipedia.org/wiki/Count-min_sketch)
7. [The Volcano Optimizer Generator](https://15721.courses.cs.cmu.edu/spring2018/papers/15-optimizer1/graefe-ieee1995.pdf)
