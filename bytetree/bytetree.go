// Package bytetree provides a Radix tree that stores []byte keys and values.
// See https://en.wikipedia.org/wiki/Radix_tree
package bytetree

import (
	"sync"
	"time"

	"github.com/getlantern/bytemap"
	"github.com/getlantern/zenodb/encoding"
	"github.com/getlantern/zenodb/sql"
)

type Tree struct {
	root   *node
	bytes  int
	length int
	mx     sync.RWMutex
}

type node struct {
	key        []byte
	edges      edges
	data       []encoding.Sequence
	removedFor []int64
}

type edge struct {
	label  []byte
	target *node
}

// New constructs a new Tree.
func New() *Tree {
	return &Tree{root: &node{}}
}

// Bytes returns an estimate of the number of bytes stored in this Tree.
func (bt *Tree) Bytes() int {
	return bt.bytes * 2
}

// Length returns the number of nodes in this Tree.
func (bt *Tree) Length() int {
	return bt.length
}

// Walk walks this Tree, calling the given fn with each node's key and data. If
// the fn returns false, the node will be removed from the Tree as viewed with
// the given ctx. Subsequent walks of the Tree using that same ctx will not see
// removed nodes, but walks using a different context will still see them.
func (bt *Tree) Walk(ctx int64, fn func(key []byte, data []encoding.Sequence) bool) {
	nodes := make([]*node, 0, bt.length)
	nodes = append(nodes, bt.root)
	for {
		if len(nodes) == 0 {
			break
		}
		n := nodes[0]
		nodes = nodes[1:]
		if n.data != nil {
			alreadyRemoved := n.wasRemovedFor(bt, ctx)
			if !alreadyRemoved {
				keep := fn(n.key, n.data)
				if !keep {
					n.doRemoveFor(bt, ctx)
				}
			}
		}
		for _, e := range n.edges {
			nodes = append(nodes, e.target)
		}
	}
}

// Remove removes the given key from this Tree under the given ctx. When viewed
// from this ctx, the key will appear to be gone, but from other contexts it
// will remain visible.
func (bt *Tree) Remove(ctx int64, fullKey []byte) []encoding.Sequence {
	// TODO: basic shape of this is very similar to update, dry violation
	n := bt.root
	key := fullKey
	// Try to update on existing edge
nodeLoop:
	for {
		for _, edge := range n.edges {
			labelLength := len(edge.label)
			keyLength := len(key)
			i := 0
			for ; i < keyLength && i < labelLength; i++ {
				if edge.label[i] != key[i] {
					break
				}
			}
			if i == keyLength && keyLength == labelLength {
				// found it
				alreadyRemoved := edge.target.wasRemovedFor(bt, ctx)
				if alreadyRemoved {
					return nil
				}
				edge.target.doRemoveFor(bt, ctx)
				return edge.target.data
			} else if i == labelLength && labelLength < keyLength {
				// descend
				n = edge.target
				key = key[labelLength:]
				continue nodeLoop
			}
		}

		// not found
		return nil
	}
}

// Copy makes a copy of this Tree.
func (bt *Tree) Copy() *Tree {
	cp := &Tree{bytes: bt.bytes, length: bt.length, root: &node{}}
	nodes := make([]*node, 0, bt.Length())
	nodeCopies := make([]*node, 0, bt.Length())
	nodes = append(nodes, bt.root)
	nodeCopies = append(nodeCopies, cp.root)

	for {
		if len(nodes) == 0 {
			break
		}
		n := nodes[0]
		cpn := nodeCopies[0]
		nodes = nodes[1:]
		nodeCopies = nodeCopies[1:]
		for _, e := range n.edges {
			cpt := &node{key: e.target.key, data: e.target.data}
			cpn.edges = append(cpn.edges, &edge{label: e.label, target: cpt})
			nodes = append(nodes, e.target)
			nodeCopies = append(nodeCopies, cpt)
		}
	}

	return cp
}

// Update updates all of the fields at the given timestamp with the given
// parameters.
func (bt *Tree) Update(fields []sql.Field, resolution time.Duration, truncateBefore time.Time, key []byte, vals encoding.TSParams, metadata bytemap.ByteMap) int {
	bytesAdded, newNode := bt.doUpdate(fields, resolution, truncateBefore, key, vals, metadata)
	bt.bytes += bytesAdded
	if newNode {
		bt.length++
	}
	return bytesAdded
}

func (bt *Tree) doUpdate(fields []sql.Field, resolution time.Duration, truncateBefore time.Time, fullKey []byte, vals encoding.TSParams, metadata bytemap.ByteMap) (int, bool) {
	n := bt.root
	key := fullKey
	// Try to update on existing edge
nodeLoop:
	for {
		for _, edge := range n.edges {
			labelLength := len(edge.label)
			keyLength := len(key)
			i := 0
			for ; i < keyLength && i < labelLength; i++ {
				if edge.label[i] != key[i] {
					break
				}
			}
			if i == keyLength && keyLength == labelLength {
				// update existing node
				return edge.target.doUpdate(fields, resolution, truncateBefore, fullKey, vals, metadata), false
			} else if i == labelLength && labelLength < keyLength {
				// descend
				n = edge.target
				key = key[labelLength:]
				continue nodeLoop
			} else if i > 0 {
				// common substring, split on that
				return edge.split(bt, fields, resolution, truncateBefore, i, fullKey, key, vals, metadata), true
			}
		}

		// Create new edge
		target := &node{key: fullKey}
		n.edges = append(n.edges, &edge{key, target})
		return target.doUpdate(fields, resolution, truncateBefore, fullKey, vals, metadata) + len(key), true
	}
}

func (n *node) doUpdate(fields []sql.Field, resolution time.Duration, truncateBefore time.Time, fullKey []byte, vals encoding.TSParams, metadata bytemap.ByteMap) int {
	bytesAdded := 0
	// Grow encoding.Sequences to match number of fields in table
	if len(n.data) < len(fields) {
		newData := make([]encoding.Sequence, len(fields))
		copy(newData, n.data)
		n.data = newData
	}
	for i, field := range fields {
		current := n.data[i]
		previousSize := cap(current)
		updated := current.Update(vals, metadata, field.Expr, resolution, truncateBefore)
		n.data[i] = updated
		bytesAdded += cap(updated) - previousSize
	}
	return bytesAdded
}

func (n *node) wasRemovedFor(bt *Tree, ctx int64) bool {
	if ctx == 0 {
		return false
	}
	bt.mx.RLock()
	for _, _ctx := range n.removedFor {
		if _ctx == ctx {
			bt.mx.RUnlock()
			return true
		}
	}
	bt.mx.RUnlock()
	return false
}

func (n *node) doRemoveFor(bt *Tree, ctx int64) {
	if ctx == 0 {
		return
	}
	bt.mx.Lock()
	n.removedFor = append(n.removedFor, ctx)
	bt.mx.Unlock()
}

func (e *edge) split(bt *Tree, fields []sql.Field, resolution time.Duration, truncateBefore time.Time, splitOn int, fullKey []byte, key []byte, vals encoding.TSParams, metadata bytemap.ByteMap) int {
	newNode := &node{edges: edges{&edge{e.label[splitOn:], e.target}}}
	newLeaf := newNode
	if splitOn != len(key) {
		newLeaf = &node{key: fullKey}
		newNode.edges = append(newNode.edges, &edge{key[splitOn:], newLeaf})
	}
	e.label = e.label[:splitOn]
	e.target = newNode
	return len(key) - splitOn + newLeaf.doUpdate(fields, resolution, truncateBefore, fullKey, vals, metadata)
}

type edges []*edge
