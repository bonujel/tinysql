# TinySQL 编码与存储技术文档

## 一、codec.EncodeInt 和 codec.DecodeInt 的实现

### 1.1 核心实现

在 `/util/codec/number.go` 中，这两个函数的实现非常精妙：

```go
// EncodeInt 将 int64 编码为可比较的字节序列
func EncodeInt(b []byte, v int64) []byte {
    var data [8]byte
    u := EncodeIntToCmpUint(v)  // 关键转换
    binary.BigEndian.PutUint64(data[:], u)
    return append(b, data[:]...)
}

// DecodeInt 解码之前编码的值
func DecodeInt(b []byte) ([]byte, int64, error) {
    if len(b) < 8 {
        return nil, 0, errors.New("insufficient bytes to decode value")
    }
    u := binary.BigEndian.Uint64(b[:8])
    v := DecodeCmpUintToInt(u)
    b = b[8:]
    return b, v, nil
}
```

### 1.2 为什么要这样编码？

关键在于 `EncodeIntToCmpUint` 函数：

```go
const signMask uint64 = 0x8000000000000000

func EncodeIntToCmpUint(v int64) uint64 {
    return uint64(v) ^ signMask
}

func DecodeCmpUintToInt(u uint64) int64 {
    return int64(u ^ signMask)
}
```

**核心思想：符号位翻转**

在计算机中，有符号整数使用补码表示：
- 正数：最高位为 0
- 负数：最高位为 1

这导致一个问题：如果直接按字节比较，负数会大于正数（因为负数最高位是 1）。

**解决方案：**
通过 XOR 操作翻转符号位（`0x8000000000000000`），使得：
- 负数的最高位从 1 变为 0
- 正数的最高位从 0 变为 1

这样字节序比较的顺序就和数值大小一致了。

### 1.3 编码示例

让我们看几个具体例子：

```
原始值: -2
二进制: 1111111111111111111111111111111111111111111111111111111111111110
XOR后:  0111111111111111111111111111111111111111111111111111111111111110

原始值: -1
二进制: 1111111111111111111111111111111111111111111111111111111111111111
XOR后:  0111111111111111111111111111111111111111111111111111111111111111

原始值: 0
二进制: 0000000000000000000000000000000000000000000000000000000000000000
XOR后:  1000000000000000000000000000000000000000000000000000000000000000

原始值: 1
二进制: 0000000000000000000000000000000000000000000000000000000000000001
XOR后:  1000000000000000000000000000000000000000000000000000000000000001

原始值: 2
二进制: 0000000000000000000000000000000000000000000000000000000000000010
XOR后:  1000000000000000000000000000000000000000000000000000000000000010
```

编码后的字节序列特点：
- 所有负数的编码都小于 `0x8000000000000000`
- 所有非负数的编码都大于等于 `0x8000000000000000`
- 在各自范围内保持原有的大小关系

### 1.4 为什么使用 BigEndian？

`binary.BigEndian.PutUint64` 将数字按大端序（高位字节在前）存储。这样做的好处是：
- 字节序比较 = 数值比较
- 第一个字节就能判断大小关系
- 适合作为 KV 存储的 Key

## 二、TinyKV 的 KV 存储模型

### 2.1 基本接口

从 `/kv/kv.go` 可以看到核心接口：

```go
// Retriever 是读取接口
type Retriever interface {
    // Get 获取指定 key 的值
    Get(ctx context.Context, k Key) ([]byte, error)

    // Iter 创建一个迭代器，从 k 开始遍历
    // 只返回 < upperBound 的 key
    Iter(k Key, upperBound Key) (Iterator, error)

    // IterReverse 创建反向迭代器
    IterReverse(k Key) (Iterator, error)
}

// Mutator 是写入接口
type Mutator interface {
    // Set 设置 key 的值
    Set(k Key, v []byte) error

    // Delete 删除 key
    Delete(k Key) error
}
```

### 2.2 Key 的排序规则

TinyKV 保证 Key 是有序的，这意味着：
1. 可以通过 `Iter(startKey)` 顺序扫描
2. 范围查询非常高效
3. Key 的字节序比较决定了顺序

### 2.3 存储层次

```
┌─────────────────────────────────────┐
│         SQL Layer (TinySQL)         │
│  - Parser                           │
│  - Optimizer                        │
│  - Executor                         │
└─────────────────────────────────────┘
              ↓
┌─────────────────────────────────────┐
│      Table Codec (编码层)           │
│  - 将表数据编码为 KV                │
│  - tableID + rowID → Key            │
└─────────────────────────────────────┘
              ↓
┌─────────────────────────────────────┐
│       KV Storage (TinyKV)           │
│  - 有序 KV 存储                     │
│  - 底层使用 RocksDB/BadgerDB        │
└─────────────────────────────────────┘
```

## 三、为什么使用有序 KV 存储

### 3.1 表数据的编码方式

从 `/tablecodec/tablecodec.go` 可以看到：

```go
// 行数据编码
// Key:   t{tableID}_r{rowID}
// Value: [col1, col2, col3, ...]

func EncodeRowKeyWithHandle(tableID int64, handle int64) kv.Key {
    buf := make([]byte, 0, RecordRowKeyLen)
    buf = appendTableRecordPrefix(buf, tableID)  // "t" + tableID
    buf = codec.EncodeInt(buf, handle)           // rowID
    return buf
}

// 索引数据编码
// Key:   t{tableID}_i{indexID}{indexColumnsValue}
// Value: rowID

func EncodeIndexSeekKey(tableID int64, idxID int64, encodedValue []byte) kv.Key {
    key := make([]byte, 0, prefixLen+idLen+len(encodedValue))
    key = appendTableIndexPrefix(key, tableID)  // "t" + tableID + "_i"
    key = codec.EncodeInt(key, idxID)
    key = append(key, encodedValue...)
    return key
}
```

### 3.2 有序存储的优势

#### 场景 1：全表扫描

```sql
SELECT * FROM users;
```

有序存储：
- 所有 `t{tableID}_r*` 的 Key 连续存储
- 一次 Scan 操作即可获取所有数据
- 时间复杂度：O(n)

无序存储（如 HashMap）：
- 需要遍历整个 HashMap
- 无法利用局部性
- 可能产生大量随机 I/O

#### 场景 2：范围查询

```sql
SELECT * FROM users WHERE id >= 100 AND id < 200;
```

有序存储：
```go
startKey := EncodeRowKeyWithHandle(tableID, 100)
endKey := EncodeRowKeyWithHandle(tableID, 200)
iter := storage.Iter(startKey, endKey)
// 只读取 [100, 200) 范围的数据
```

- 直接定位到起始位置
- 顺序读取到结束位置
- 时间复杂度：O(log n + m)，m 是结果数量

无序存储：
- 必须扫描全表
- 时间复杂度：O(n)

#### 场景 3：索引查询

```sql
SELECT * FROM users WHERE name = 'Alice';
```

有序存储：
```go
// 1. 通过索引找到 rowID
indexKey := EncodeIndexSeekKey(tableID, nameIndexID, "Alice")
rowID := storage.Get(indexKey)

// 2. 通过 rowID 获取完整行数据
rowKey := EncodeRowKeyWithHandle(tableID, rowID)
rowData := storage.Get(rowKey)
```

- 两次精确查找
- 时间复杂度：O(log n)

#### 场景 4：排序操作

```sql
SELECT * FROM users ORDER BY id;
```

有序存储：
- 数据本身就是按 rowID 排序的
- 直接顺序扫描即可
- 无需额外排序

无序存储：
- 需要读取所有数据后排序
- 时间复杂度：O(n log n)

### 3.3 对比总结

| 操作类型 | 有序存储 | 无序存储 (HashMap) |
|---------|---------|-------------------|
| 点查询 | O(log n) | O(1) 平均 |
| 范围查询 | O(log n + m) | O(n) |
| 全表扫描 | O(n) 顺序 I/O | O(n) 随机 I/O |
| 排序查询 | O(n) | O(n log n) |
| 前缀扫描 | O(log n + m) | O(n) |

### 3.4 实际应用场景

**适合有序存储的场景：**
- 数据库系统（范围查询频繁）
- 时序数据（按时间顺序访问）
- 日志系统（顺序写入，范围读取）
- 文件系统（目录结构）

**适合无序存储的场景：**
- 纯 KV 缓存（只有点查询）
- Session 存储
- 配置中心

## 四、字节编码的其他技巧

### 4.1 字节数组编码

从 `/util/codec/bytes.go` 可以看到：

```go
// EncodeBytes 保证编码后的字节序列可比较
// 规则：[group1][marker1]...[groupN][markerN]
// group 是 8 字节，不足的用 0 填充
// marker 是 0xFF - 填充的 0 的数量

// 例子：
// []           -> [0,0,0,0,0,0,0,0,247]
// [1,2,3]      -> [1,2,3,0,0,0,0,0,250]
// [1,2,3,0]    -> [1,2,3,0,0,0,0,0,251]
// [1,2,3,4,5,6,7,8] -> [1,2,3,4,5,6,7,8,255,0,0,0,0,0,0,0,0,247]
```

这种编码方式的好处：
1. 保持字节序可比较性
2. 可以处理任意长度的字节数组
3. 可以区分尾部的 0 是数据还是填充

### 4.2 变长整数编码

```go
// EncodeVarint 使用变长编码（不保证可比较）
func EncodeVarint(b []byte, v int64) []byte {
    var data [binary.MaxVarintLen64]byte
    n := binary.PutVarint(data[:], v)
    return append(b, data[:n]...)
}
```

变长编码适合：
- Value 部分（不需要比较）
- 节省空间
- 小数字占用更少字节

## 五、总结

TinySQL 的编码设计体现了几个核心思想：

1. **有序性优先**：通过精心设计的编码方式，保证字节序比较 = 逻辑比较
2. **分层设计**：SQL 层 → 编码层 → KV 层，各司其职
3. **空间与性能的权衡**：Key 使用固定长度编码（可比较），Value 使用变长编码（节省空间）
4. **利用底层特性**：充分利用有序 KV 存储的特性，优化数据库操作

这种设计使得 TinySQL 能够高效地支持各种 SQL 操作，特别是范围查询和排序操作。
