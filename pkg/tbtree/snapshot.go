/*
Copyright 2019-2020 vChain, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package tbtree

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

var ErrReadersNotClosed = errors.New("readers not closed")

const (
	InnerNodeType = iota
	RootInnerNodeType
	LeafNodeType
	RootLeafNodeType
)

type Snapshot struct {
	t           *TBtree
	id          uint64
	root        node
	readers     map[int]*Reader
	maxReaderID int
	closed      bool
	rwmutex     sync.RWMutex
}

func (s *Snapshot) Get(key []byte) (value []byte, ts uint64, err error) {
	s.rwmutex.RLock()
	defer s.rwmutex.RUnlock()

	if s.closed {
		return nil, 0, ErrAlreadyClosed
	}

	return s.root.get(key)
}

func (s *Snapshot) Ts() (uint64, error) {
	s.rwmutex.RLock()
	defer s.rwmutex.RUnlock()

	if s.closed {
		return 0, ErrAlreadyClosed
	}

	return s.root.ts(), nil
}

func (s *Snapshot) Reader(spec *ReaderSpec) (*Reader, error) {
	s.rwmutex.RLock()
	defer s.rwmutex.RUnlock()

	if s.closed {
		return nil, ErrAlreadyClosed
	}

	if spec == nil {
		return nil, ErrIllegalArgument
	}

	path, startingLeaf, startingOffset, err := s.root.findLeafNode(spec.initialKey, nil, nil, spec.ascOrder)
	if err == ErrKeyNotFound {
		return nil, ErrNoMoreEntries
	}
	if err != nil {
		return nil, err
	}

	reader := &Reader{
		snapshot:   s,
		id:         s.maxReaderID,
		initialKey: spec.initialKey,
		isPrefix:   spec.isPrefix,
		ascOrder:   spec.ascOrder,
		path:       path,
		leafNode:   startingLeaf,
		offset:     startingOffset,
		closed:     false,
	}

	s.readers[reader.id] = reader

	s.maxReaderID++

	return reader, nil
}

func (s *Snapshot) closedReader(r *Reader) error {
	s.rwmutex.Lock()
	defer s.rwmutex.Unlock()

	if s.closed {
		return ErrAlreadyClosed
	}

	delete(s.readers, r.id)

	return nil
}

func (s *Snapshot) Close() error {
	s.rwmutex.Lock()
	defer s.rwmutex.Unlock()

	if s.closed {
		return ErrAlreadyClosed
	}

	if len(s.readers) > 0 {
		return ErrReadersNotClosed
	}

	err := s.t.snapshotClosed(s)
	if err != nil {
		return err
	}

	s.closed = true

	return nil
}

func (s *Snapshot) WriteTo(w io.Writer, onlyMutated bool, onlyLatest bool) error {
	return s.root.writeTo(w, onlyMutated, onlyLatest)
}

func (n *innerNode) writeTo(w io.Writer, onlyMutated bool, onlyLatest bool) error {
	if onlyMutated && n.off > 0 {
		//TODO: let node manager know this node can be recycled
		return nil
	}

	if !onlyLatest && n.prevNode != nil {
		err := n.prevNode.writeTo(w, onlyMutated, onlyLatest)
		if err != nil {
			return err
		}
	}

	for _, c := range n.nodes {
		if onlyMutated && !c.node.mutated() {
			continue
		}

		err := c.node.writeTo(w, onlyMutated, onlyLatest)
		if err != nil {
			return err
		}
	}

	buf := make([]byte, n.size())
	i := 0

	if n.prevNode == nil {
		buf[i] = InnerNodeType
		i++

		binary.LittleEndian.PutUint32(buf[i:], uint32(len(buf))) // Size
		i += 4
	} else {
		buf[i] = RootInnerNodeType
		i++
	}

	if !onlyLatest && n.prevNode != nil {
		binary.LittleEndian.PutUint64(buf[i:], n.prevNode.offset())
		i += 8
	}

	binary.LittleEndian.PutUint32(buf[i:], uint32(len(n.nodes)))
	i += 4

	for _, c := range n.nodes {
		binary.LittleEndian.PutUint32(buf[i:], uint32(len(c.key)))
		i += 4

		copy(buf[i:], c.key)
		i += len(c.key)

		binary.LittleEndian.PutUint64(buf[i:], c.cts)
		i += 8

		binary.LittleEndian.PutUint64(buf[i:], c.node.offset())
		i += 8
	}

	if n.prevNode != nil {
		binary.LittleEndian.PutUint32(buf[i:], uint32(len(buf))) // Size
		i += 4

		// TODO: calculate hash
		i += 32
	}

	//TODO: let node manager know this node can be recycled

	return writeTo(buf, w)
}

func (l *leafNode) writeTo(w io.Writer, onlyMutated bool, onlyLatest bool) error {
	if onlyMutated && l.off > 0 {
		//TODO: let node manager know this node can be recycled
		return nil
	}

	if !onlyLatest && l.prevNode != nil {
		err := l.prevNode.writeTo(w, onlyMutated, onlyLatest)
		if err != nil {
			return err
		}
	}

	buf := make([]byte, l.size())
	i := 0

	if l.prevNode == nil {
		buf[i] = LeafNodeType
		i++

		binary.LittleEndian.PutUint32(buf[i:], uint32(len(buf))) // Size
		i += 4
	} else {
		buf[i] = RootLeafNodeType
		i++
	}

	if !onlyLatest && l.prevNode != nil {
		binary.LittleEndian.PutUint64(buf[i:], l.prevNode.offset())
		i += 8
	}

	binary.LittleEndian.PutUint32(buf[i:], uint32(len(l.values)))
	i += 4

	for _, v := range l.values {
		binary.LittleEndian.PutUint32(buf[i:], uint32(len(v.key)))
		i += 4

		copy(buf[i:], v.key)
		i += len(v.key)

		binary.LittleEndian.PutUint32(buf[i:], uint32(len(v.value)))
		i += 4

		copy(buf[i:], v.value)
		i += len(v.value)

		binary.LittleEndian.PutUint64(buf[i:], v.ts)
		i += 8

		binary.LittleEndian.PutUint64(buf[i:], v.prevTs)
		i += 8
	}

	if l.prevNode != nil {
		binary.LittleEndian.PutUint32(buf[i:], uint32(len(buf))) // Size
		i += 4

		// TODO: calculate hash
		i += 32
	}

	//TODO: let node manager know this node can be recycled

	return writeTo(buf, w)
}

func (n *nodeRef) writeTo(w io.Writer, onlyMutated bool, onlyLatest bool) error {
	if !onlyMutated {
		return nil
	}

	//TODO: load from nodeManager and call writeTo

	return nil
}

func writeTo(buf []byte, w io.Writer) error {
	wn := 0
	for {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		wn += n

		if len(buf) == wn {
			break
		}
	}
	return nil
}
