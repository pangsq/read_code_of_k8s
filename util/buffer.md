# buffer

## RingGrowing

可增长的环形缓冲区，非线程安全，源码在k8s.io/utils/buffer/ring_growing.go

版本

```
branch master
commit 2df71ebbae66f39338aed4cd0bb82d2212ee33cc
Date:   Tue Apr 14 03:07:11 2020 -0700
```

### 结构体与实例化方法

1. data []interface{} // 存放数据的数组
2. n int              // data的容量
3. beg                // 第一个可读的数据所在位置
4. readable           // 可读的数据数量

```go
type RingGrowing struct {
	data     []interface{}
	n        int // Size of Data
	beg      int // First available element
	readable int // Number of data items available
}

func NewRingGrowing(initialSize int) *RingGrowing {
	return &RingGrowing{
		data: make([]interface{}, initialSize),
		n:    initialSize,
	}
}
```

### 方法

1. ReadOne() (data interface{}, ok bool) // 读取一个数据，并返回是否读成功的判断
2. WriteOne(data interface{})            // 写一个数据

```go
// ReadOne reads (consumes) first item from the buffer if it is available, otherwise returns false.
func (r *RingGrowing) ReadOne() (data interface{}, ok bool) {
	if r.readable == 0 {
		return nil, false
	}
	r.readable--
	element := r.data[r.beg]
	// 考虑gc
	r.data[r.beg] = nil // Remove reference to the object to help GC
	if r.beg == r.n-1 {
		// Was the last element // 环形buffer特点，到数组尾部之后即是数组头部
		r.beg = 0
	} else {
		r.beg++
	}
	return element, true
}

// WriteOne adds an item to the end of the buffer, growing it if it is full.
func (r *RingGrowing) WriteOne(data interface{}) {
    // 插入数据前先判断是否需要扩容buffer
	if r.readable == r.n {
		// Time to grow
		newN := r.n * 2 // 指数增长
		newData := make([]interface{}, newN)
		to := r.beg + r.readable
		if to <= r.n { // 原缓冲区中可读数据为一段相连，不过尾部
			copy(newData, r.data[r.beg:to])
		} else { // 若原缓冲区中可读数据过尾部，需要分两端拷贝
			copied := copy(newData, r.data[r.beg:])
			copy(newData[copied:], r.data[:(to%r.n)])
		}
		r.beg = 0
		r.data = newData
		r.n = newN
	}
	r.data[(r.readable+r.beg)%r.n] = data
	r.readable++
}
```