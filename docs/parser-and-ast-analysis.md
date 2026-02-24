          root
         /  |  \
        S   F   W
        |   |   |
        E   R   H
        |   |   |
        L   O   E
        |   |   |
        E   M   R
        |       |
        C       E
        |
        T
```

当扫描到"SELECT"时，沿着树路径匹配，快速确定这是一个关键字token。

---

## 语法分析器（Parser）

### BNF语法规则

BNF（Backus-Naur Form）是描述上下文无关语法的标准记法。

**示例**：SELECT语句的简化BNF
```bnf
SelectStmt ::= "SELECT" FieldList "FROM" TableRefs WhereClause

FieldList  ::= "*" | ColumnName ("," ColumnName)*

TableRefs  ::= TableRef | TableRef "," TableRefs

WhereClause ::= ε | "WHERE" Expression
```

### parser.y文件结构

```yacc
%union {
    // 定义堆栈项的类型
}
%%
// 语法规则部分
SelectStmt:
    "SELECT" FieldList "FROM" TableRefs
    {
        // action: 构建AST节点
        $$ = &ast.SelectStmt{...}
    }
%%
// subroutines部分（TinySQL中为空）
```

### 归约过程（Shift-Reduce）

Parser使用自底向上的归约方式解析：

**示例**：解析 `SELECT a FROM t`

```
步骤  堆栈内容              输入剩余           动作
1     .                    SELECT a FROM t    shift SELECT
2     SELECT .             a FROM t           shift a
3     SELECT a .           FROM t             reduce: a → ColumnName
4     SELECT ColumnName .  FROM t             reduce: ColumnName → FieldList
5     SELECT FieldList .   FROM t             shift FROM
6     SELECT FieldList FROM . t              shift t
7     SELECT FieldList FROM t .              reduce: t → TableRef
8     SELECT FieldList FROM TableRef .       reduce: 整个规则 → SelectStmt
```

### $符号的含义

在action代码中：
- `$1`, `$2`, `$3`... 引用产生式右侧的第1、2、3...项
- `$$` 代表产生式左侧的非终结符（归约后的结果）

**示例**：
```yacc
JoinTable:
    TableRef "JOIN" TableRef
    {
        // $1 = 第一个TableRef
        // $2 = "JOIN" token
        // $3 = 第二个TableRef
        // $$ = 归约后的JoinTable
        $$ = &ast.Join{Left: $1, Right: $3}
    }
```

---

## 抽象语法树（AST）

### AST的本质

AST是源代码的树形表示，去除了语法细节，保留了语义结构。

**SQL**：
```sql
SELECT a FROM t WHERE b > 0
```

**对应的AST**：
```
        SelectStmt
       /    |    \
      /     |     \
  Fields  From   Where
    |       |      |
    a       t    b > 0
                /  |  \
               b   >   0
```

### TinySQL的AST节点

所有AST节点都实现`ast.Node`接口：

```go
// Node is the basic element of the AST
type Node interface {
    Accept(v Visitor) (node Node, ok bool)
    Text() string
    SetText(text string)
}
```

**关键接口**：
- `ExprNode`：表达式节点（如 `a + b`）
- `StmtNode`：语句节点（如 `SELECT`）
- `ResultSetNode`：结果集节点（如表引用）

### Visitor模式

AST使用Visitor模式进行遍历和转换：

```go
type Visitor interface {
    Enter(n Node) (node Node, skipChildren bool)
    Leave(n Node) (node Node, ok bool)
}
```

**遍历过程**：
1. 调用`Enter`进入节点
2. 如果`skipChildren=false`，递归访问子节点
3. 调用`Leave`离开节点

**应用场景**：
- 语义检查（验证表名、列名是否存在）
- 类型推导（计算表达式的数据类型）
- 查询优化（重写AST以提高性能）

---

## JoinTable 实现详解

### MySQL JOIN语法

MySQL支持多种JOIN类型：

```sql
-- CROSS JOIN (笛卡尔积)
SELECT * FROM t1, t2
SELECT * FROM t1 JOIN t2
SELECT * FROM t1 CROSS JOIN t2

-- INNER JOIN (内连接)
SELECT * FROM t1 INNER JOIN t2 ON t1.id = t2.id

-- LEFT JOIN (左外连接)
SELECT * FROM t1 LEFT JOIN t2 ON t1.id = t2.id
SELECT * FROM t1 LEFT OUTER JOIN t2 ON t1.id = t2.id

-- RIGHT JOIN (右外连接)
SELECT * FROM t1 RIGHT JOIN t2 ON t1.id = t2.id
SELECT * FROM t1 RIGHT OUTER JOIN t2 ON t1.id = t2.id
```

### AST节点定义

```go
// JoinType represents join type
type JoinType int

const (
    CrossJoin JoinType = iota + 1  // CROSS JOIN
    LeftJoin                        // LEFT JOIN
    RightJoin                       // RIGHT JOIN
)

// Join represents table join
type Join struct {
    node

    Left  ResultSetNode  // 左表
    Right ResultSetNode  // 右表
    Tp    JoinType       // JOIN类型
    On    *OnCondition   // ON条件（可选）
}

// OnCondition represents ON condition in JOIN
type OnCondition struct {
    node
    Expr ExprNode  // ON后面的表达式
}
```

### parser.y中的实现

#### 1. CROSS JOIN实现

```yacc
JoinTable:
    TableRef CrossOpt TableRef %prec tableRefPriority
    {
        $$ = &ast.Join{
            Left:  $1.(ast.ResultSetNode),
            Right: $3.(ast.ResultSetNode),
            Tp:    ast.CrossJoin
        }
    }

CrossOpt:
    "JOIN"
|   "INNER" "JOIN"
```

**解析示例**：
```sql
SELECT * FROM t1 JOIN t2
```
- `$1` = t1 (TableRef)
- `$3` = t2 (TableRef)
- 构造CrossJoin节点

#### 2. LEFT/RIGHT JOIN实现（Project 2任务）

```yacc
|   TableRef JoinType OuterOpt "JOIN" TableRef "ON" Expression
    {
        $$ = &ast.Join{
            Left:  $1.(ast.ResultSetNode),
            Right: $5.(ast.ResultSetNode),
            Tp:    $2.(ast.JoinType),
            On:    &ast.OnCondition{Expr: $7.(ast.ExprNode)}
        }
    }

JoinType:
    "LEFT"  { $$ = ast.LeftJoin }
|   "RIGHT" { $$ = ast.RightJoin }

OuterOpt:
    {}        // 可选的OUTER关键字
|   "OUTER"
```

**解析示例**：
```sql
SELECT * FROM t1 LEFT JOIN t2 ON t1.id = t2.id
```
- `$1` = t1 (TableRef)
- `$2` = LeftJoin (JoinType)
- `$3` = (OuterOpt，可能为空)
- `$4` = "JOIN" token
- `$5` = t2 (TableRef)
- `$6` = "ON" token
- `$7` = t1.id = t2.id (Expression)

**构造的AST**：
```
Join {
    Left: TableSource(t1),
    Right: TableSource(t2),
    Tp: LeftJoin,
    On: OnCondition {
        Expr: BinaryOperation {
            Op: "=",
            L: ColumnName(t1.id),
            R: ColumnName(t2.id)
        }
    }
}
```

### JOIN的左结合性

多个JOIN操作是左结合的：

```sql
SELECT * FROM t1 JOIN t2 LEFT JOIN t3 ON t2.id = t3.id
```

**解析过程**：
1. 先归约 `t1 JOIN t2` → `Join1`
2. 再归约 `Join1 LEFT JOIN t3 ON ...` → `Join2`

**AST结构**：
```
        Join2 (LEFT JOIN)
       /                \
    Join1              t3
   /     \
  t1     t2
```

这种左结合性是yacc默认的归约规则决定的。

---

## 实战案例分析

### 案例1：简单的LEFT JOIN

**SQL**：
```sql
SELECT * FROM users LEFT JOIN orders ON users.id = orders.user_id
```

**Token流**：
```
SELECT, *, FROM, users, LEFT, JOIN, orders, ON,
users, ., id, =, orders, ., user_id
```

**AST构建过程**：

1. **解析SELECT部分**：
```go
SelectStmt {
    Fields: FieldList { Fields: [WildCard] },
    From: ...  // 待构建
}
```

2. **解析FROM子句**：
```go
TableRefsClause {
    TableRefs: Join {  // 这是关键
        Left: TableSource {
            Source: TableName { Name: "users" }
        },
        Right: TableSource {
            Source: TableName { Name: "orders" }
        },
        Tp: LeftJoin,
        On: OnCondition {
            Expr: BinaryOperation {
                Op: "=",
                L: ColumnName { Table: "users", Name: "id" },
                R: ColumnName { Table: "orders", Name: "user_id" }
            }
        }
    }
}
```

3. **完整AST**：
```
SelectStmt
├── Fields: [*]
├── From: TableRefsClause
│   └── TableRefs: Join (LEFT)
│       ├── Left: TableSource(users)
│       ├── Right: TableSource(orders)
│       └── On: users.id = orders.user_id
└── Where: nil
```

### 案例2：多表JOIN

**SQL**：
```sql
SELECT * FROM t1
JOIN t2
LEFT JOIN t3 ON t2.id = t3.id
RIGHT JOIN t4 ON t3.id = t4.id
```

**AST结构**（左结合）：
```
                Join4 (RIGHT)
               /            \
          Join3 (LEFT)       t4
         /           \
    Join2 (CROSS)     t3
   /            \
  t1            t2
```

**归约步骤**：
```
1. t1 JOIN t2           → Join2 (CROSS)
2. Join2 LEFT JOIN t3   → Join3 (LEFT)
3. Join3 RIGHT JOIN t4  → Join4 (RIGHT)
```

### 案例3：复杂的ON条件

**SQL**：
```sql
SELECT * FROM t1
LEFT JOIN t2 ON t1.id = t2.id AND t1.status = 'active'
```

**ON条件的AST**：
```go
OnCondition {
    Expr: BinaryOperation {
        Op: "AND",
        L: BinaryOperation {
            Op: "=",
            L: ColumnName(t1.id),
            R: ColumnName(t2.id)
        },
        R: BinaryOperation {
            Op: "=",
            L: ColumnName(t1.status),
            R: StringLiteral("active")
        }
    }
}
```

**树形表示**：
```
        AND
       /   \
      =     =
     / \   / \
   t1.id  t1.status
   t2.id  'active'
```

---

## 关键技术点总结

### 1. 为什么使用Lex & Yacc？

**优点**：
- **声明式**：只需定义规则，不需要手写解析逻辑
- **高效**：生成的解析器性能优秀
- **可维护**：语法规则集中管理，易于修改

**缺点**：
- **学习曲线**：需要理解编译原理
- **调试困难**：生成的代码难以阅读
- **错误信息**：语法错误提示不够友好

### 2. goyacc vs 手写Parser

**goyacc**：
- 适合复杂语法（如SQL）
- 自动处理优先级和结合性
- 生成的代码性能好

**手写Parser**：
- 适合简单语法
- 错误处理更灵活
- 代码可读性好

TinySQL选择goyacc是因为SQL语法极其复杂，手写Parser工作量巨大。

### 3. AST vs Parse Tree

**Parse Tree（解析树）**：
- 包含所有语法细节
- 节点对应每个语法规则
- 冗余信息多

**AST（抽象语法树）**：
- 去除语法细节
- 只保留语义信息
- 结构更简洁

**示例**：`1 + 2 * 3`

Parse Tree:
```
      Expression
          |
      Addition
     /    |    \
    1     +   Multiplication
              /   |   \
             2    *    3
```

AST:
```
      +
     / \
    1   *
       / \
      2   3
```

### 4. 优先级和结合性

在parser.y中定义：

```yacc
%left  "OR"           // 左结合，低优先级
%left  "AND"          // 左结合
%left  "=" "!="       // 左结合
%left  "+" "-"        // 左结合
%left  "*" "/" "%"    // 左结合，高优先级
%right "NOT"          // 右结合
```

**作用**：
- 解决语法歧义
- 确定运算顺序
- 避免shift/reduce冲突

### 5. 性能优化

**Lexer优化**：
- 使用字典树快速匹配关键字
- 缓冲区减少IO操作
- 预分配内存避免频繁分配

**Parser优化**：
- LR(1)解析表，O(n)时间复杂度
- 堆栈操作高效
- 避免回溯

---

## 扩展阅读

### 相关概念

1. **LL vs LR解析器**
   - LL：自顶向下，递归下降
   - LR：自底向上，移进归约（yacc使用）

2. **LALR(1)解析器**
   - yacc生成的解析器类型
   - Look-Ahead LR，向前看1个token
   - 比LR(1)更节省内存

3. **Shift/Reduce冲突**
   - Shift：将token压入堆栈
   - Reduce：按规则归约
   - 冲突：不确定该shift还是reduce
   - 解决：使用优先级和结合性

### 实践建议

1. **修改语法规则**：
   - 先理解现有规则
   - 参考MySQL文档
   - 运行测试验证

2. **调试技巧**：
   - 查看`y.output`文件（解析表）
   - 使用`-v`选项生成详细信息
   - 添加打印语句跟踪

3. **常见错误**：
   - 忘记添加`|`分隔多个产生式
   - `$$`和`$n`使用错误
   - 类型转换错误（需要类型断言）

---

## 总结

通过Project 2，我们学习了：

1. **编译原理基础**：词法分析、语法分析、AST
2. **Lex & Yacc工具**：如何使用goyacc生成解析器
3. **SQL解析流程**：从文本到AST的完整过程
4. **JOIN语法实现**：如何添加新的语法规则
5. **AST遍历**：Visitor模式的应用

**核心收获**：
- 理解了数据库如何"理解"SQL语句
- 掌握了使用yacc定义语法规则的方法
- 学会了构建和操作AST

**下一步**：
- Project 3：DDL（数据定义语言）
- Project 4：查询优化器
- Project 5：执行器

SQL解析是数据库的第一步，为后续的优化和执行奠定了基础。

---

## 参考资料

- [MySQL 8.0 Reference Manual - SQL Statements](https://dev.mysql.com/doc/refman/8.0/en/sql-statements.html)
- [goyacc Documentation](https://github.com/cznic/goyacc)
- [Lex & Yacc Tutorial](http://dinosaur.compilertools.net/)
- [TiDB Source Code Reading - Parser](https://pingcap.com/zh/blog/tidb-source-code-reading-5)
- [Compilers: Principles, Techniques, and Tools (Dragon Book)](https://en.wikipedia.org/wiki/Compilers:_Principles,_Techniques,_and_Tools)
