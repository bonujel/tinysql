// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/parser/ast"
)

type joinReorderDPSolver struct {
	*baseSingleGroupJoinOrderSolver
	newJoin func(lChild, rChild LogicalPlan, eqConds []*expression.ScalarFunction, otherConds []expression.Expression) LogicalPlan
}

type joinGroupEqEdge struct {
	nodeIDs []int
	edge    *expression.ScalarFunction
}

type joinGroupNonEqEdge struct {
	nodeIDs    []int
	nodeIDMask uint
	expr       expression.Expression
}

func (s *joinReorderDPSolver) solve(joinGroup []LogicalPlan, eqConds []expression.Expression) (LogicalPlan, error) {
	// 节点数量
	n := len(joinGroup)
	if n == 0 {
		return nil, errors.Errorf("empty join group")
	}
	if n == 1 {
		return joinGroup[0], nil
	}

	// 构建边的映射关系
	eqEdgeMap := make(map[int][]joinGroupEqEdge)
	for _, cond := range eqConds {
		sf := cond.(*expression.ScalarFunction)
		lCol := sf.GetArgs()[0].(*expression.Column)
		rCol := sf.GetArgs()[1].(*expression.Column)
		lIdx, err := findNodeIndexInGroup(joinGroup, lCol)
		if err != nil {
			return nil, err
		}
		rIdx, err := findNodeIndexInGroup(joinGroup, rCol)
		if err != nil {
			return nil, err
		}
		// 为两个节点之间的边建立映射
		nodeMask := (1 << uint(lIdx)) | (1 << uint(rIdx))
		eqEdgeMap[nodeMask] = append(eqEdgeMap[nodeMask], joinGroupEqEdge{
			nodeIDs: []int{lIdx, rIdx},
			edge:    sf,
		})
	}

	// 初始化 DP 表：dpTable[节点集合] = 最优计划
	// costTable 存储每个节点集合的累积代价
	dpTable := make(map[uint]LogicalPlan)
	costTable := make(map[uint]float64)

	// 初始化单节点状态
	for i := 0; i < n; i++ {
		_, err := joinGroup[i].recursiveDeriveStats()
		if err != nil {
			return nil, err
		}
		mask := uint(1 << uint(i))
		dpTable[mask] = joinGroup[i]
		costTable[mask] = s.baseNodeCumCost(joinGroup[i])
	}

	// DP 主循环：枚举集合大小
	for size := 2; size <= n; size++ {
		// 枚举大小为 size 的所有子集
		for group := uint(0); group < (1 << uint(n)); group++ {
			// 检查集合大小是否为 size
			if countOnes(group) != size {
				continue
			}

			var bestPlan LogicalPlan
			var minCost float64 = -1
			var bestLeftMask uint

			// 当总代价相同，优先选择左子集规模更小的方案，
			// 若规模相同，则选择位掩码更小的方案，保证结果稳定且倾向右深树。
			preferLeft := func(leftMask, curBestLeftMask uint) bool {
				leftSize := countOnes(leftMask)
				bestSize := countOnes(curBestLeftMask)
				if leftSize != bestSize {
					return leftSize < bestSize
				}
				return leftMask < curBestLeftMask
			}

			// 枚举子集划分
			// 使用 Gosper's Hack 枚举 group 的所有非空真子集
			for sub := (group - 1) & group; sub > 0; sub = (sub - 1) & group {
				complement := group ^ sub

				// 检查 sub 和 complement 是否都在 dpTable 中
				leftPlan, leftOK := dpTable[sub]
				rightPlan, rightOK := dpTable[complement]
				if !leftOK || !rightOK {
					continue
				}

				// 检查是否有连接边
				edges := s.findUsableEdges(sub, complement, eqEdgeMap)
				if len(edges) == 0 {
					// 没有连接边，跳过（笛卡尔积在后面处理）
					continue
				}

				// 尝试两种连接顺序：left join right 和 right join left
				// 1. left join right
				newJoin1, err := s.newJoinWithEdge(leftPlan, rightPlan, edges, s.otherConds)
				if err != nil {
					return nil, err
				}
				leftCost := costTable[sub]
				rightCost := costTable[complement]
				joinCost1 := newJoin1.statsInfo().RowCount
				totalCost1 := leftCost + rightCost + joinCost1

					if minCost < 0 || totalCost1 < minCost || (totalCost1 == minCost && preferLeft(sub, bestLeftMask)) {
						minCost = totalCost1
						bestPlan = newJoin1
						bestLeftMask = sub
					}

				// 2. right join left
				newJoin2, err := s.newJoinWithEdge(rightPlan, leftPlan, edges, s.otherConds)
				if err != nil {
					return nil, err
				}
				joinCost2 := newJoin2.statsInfo().RowCount
				totalCost2 := leftCost + rightCost + joinCost2

					if totalCost2 < minCost || (totalCost2 == minCost && preferLeft(complement, bestLeftMask)) {
						minCost = totalCost2
						bestPlan = newJoin2
						bestLeftMask = complement
					}
			}

			// 如果找到了最优计划，更新 DP 表
			if bestPlan != nil {
				dpTable[group] = bestPlan
				costTable[group] = minCost
			}
		}
	}

	// 处理断开的连接图（笛卡尔积）
	// 使用 BFS 找到所有连通分量
	fullSet := (1 << uint(n)) - 1
	if _, ok := dpTable[uint(fullSet)]; ok {
		// 所有节点连通，直接返回
		return dpTable[uint(fullSet)], nil
	}

	// 找到所有连通分量
	components := s.findConnectedComponents(uint(fullSet), dpTable)
	if len(components) == 0 {
		return nil, errors.Errorf("no connected components found")
	}

	// 使用 makeBushyJoin 合并连通分量
	cartesianGroup := make([]LogicalPlan, 0, len(components))
	for _, comp := range components {
		cartesianGroup = append(cartesianGroup, dpTable[comp])
	}

	return s.makeBushyJoin(cartesianGroup, s.otherConds), nil
}

func (s *joinReorderDPSolver) newJoinWithEdge(leftPlan, rightPlan LogicalPlan, edges []joinGroupEqEdge, otherConds []expression.Expression) (LogicalPlan, error) {
	var eqConds []*expression.ScalarFunction
	for _, edge := range edges {
		lCol := edge.edge.GetArgs()[0].(*expression.Column)
		rCol := edge.edge.GetArgs()[1].(*expression.Column)
		if leftPlan.Schema().Contains(lCol) {
			eqConds = append(eqConds, edge.edge)
		} else {
			newSf := expression.NewFunctionInternal(s.ctx, ast.EQ, edge.edge.GetType(), rCol, lCol).(*expression.ScalarFunction)
			eqConds = append(eqConds, newSf)
		}
	}
	join := s.newJoin(leftPlan, rightPlan, eqConds, otherConds)
	_, err := join.recursiveDeriveStats()
	return join, err
}

// Make cartesian join as bushy tree.
func (s *joinReorderDPSolver) makeBushyJoin(cartesianJoinGroup []LogicalPlan, otherConds []expression.Expression) LogicalPlan {
	for len(cartesianJoinGroup) > 1 {
		resultJoinGroup := make([]LogicalPlan, 0, len(cartesianJoinGroup))
		for i := 0; i < len(cartesianJoinGroup); i += 2 {
			if i+1 == len(cartesianJoinGroup) {
				resultJoinGroup = append(resultJoinGroup, cartesianJoinGroup[i])
				break
			}
			// TODO:Since the other condition may involve more than two tables, e.g. t1.a = t2.b+t3.c.
			//  So We'll need a extra stage to deal with it.
			// Currently, we just add it when building cartesianJoinGroup.
			mergedSchema := expression.MergeSchema(cartesianJoinGroup[i].Schema(), cartesianJoinGroup[i+1].Schema())
			var usedOtherConds []expression.Expression
			otherConds, usedOtherConds = expression.FilterOutInPlace(otherConds, func(expr expression.Expression) bool {
				return expression.ExprFromSchema(expr, mergedSchema)
			})
			resultJoinGroup = append(resultJoinGroup, s.newJoin(cartesianJoinGroup[i], cartesianJoinGroup[i+1], nil, usedOtherConds))
		}
		cartesianJoinGroup = resultJoinGroup
	}
	return cartesianJoinGroup[0]
}

func findNodeIndexInGroup(group []LogicalPlan, col *expression.Column) (int, error) {
	for i, plan := range group {
		if plan.Schema().Contains(col) {
			return i, nil
		}
	}
	return -1, ErrUnknownColumn.GenWithStackByArgs(col, "JOIN REORDER RULE")
}

// countOnes 计算二进制表示中 1 的个数
func countOnes(n uint) int {
	count := 0
	for n > 0 {
		count++
		n &= n - 1
	}
	return count
}

// findUsableEdges 查找连接两个节点集合的所有边
func (s *joinReorderDPSolver) findUsableEdges(left, right uint, eqEdgeMap map[int][]joinGroupEqEdge) []joinGroupEqEdge {
	var edges []joinGroupEqEdge
	// 遍历所有边，检查是否连接 left 和 right
	for mask, edgeList := range eqEdgeMap {
		// mask 是两个节点的位掩码，如 (1<<i) | (1<<j)
		// 检查这两个节点是否分别在 left 和 right 中
		maskUint := uint(mask)

		// 计算 mask 中有多少位在 left 中
		leftBits := maskUint & left
		// 计算 mask 中有多少位在 right 中
		rightBits := maskUint & right

		// 如果 mask 的两个节点分别在 left 和 right 中，则这条边可用
		// 即：leftBits 和 rightBits 都不为 0，且它们的并集等于 mask
		if leftBits != 0 && rightBits != 0 && (leftBits|rightBits) == maskUint {
			edges = append(edges, edgeList...)
		}
	}
	return edges
}

// findConnectedComponents 找到所有连通分量
func (s *joinReorderDPSolver) findConnectedComponents(fullSet uint, dpTable map[uint]LogicalPlan) []uint {
	var components []uint
	visited := uint(0)

	// 遍历所有可能的子集，找到最大的连通分量
	for visited != fullSet {
		// 找到第一个未访问的节点
		var start uint
		for i := uint(0); i < 32; i++ {
			if (fullSet & (1 << i)) != 0 && (visited & (1 << i)) == 0 {
				start = 1 << i
				break
			}
		}

		// 从 start 开始 BFS，找到所有连通的节点
		component := start
		queue := []uint{start}
		visited |= start

		for len(queue) > 0 {
			queue = queue[1:]

			// 尝试扩展到其他未访问的节点
			for i := uint(0); i < 32; i++ {
				node := uint(1 << i)
				if (fullSet & node) == 0 || (visited & node) != 0 {
					continue
				}

				// 检查 component 和 node 是否可以连接
				newSet := component | node
				if _, ok := dpTable[newSet]; ok {
					component = newSet
					visited |= node
					queue = append(queue, node)
				}
			}
		}

		components = append(components, component)
	}

	return components
}
