/*
Copyright (c) 2021 Simon Schmidt

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

/*
Stores reverse index data into a LevelDB database.
*/
package rildb


import (
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	//"github.com/syndtr/goleveldb/leveldb/util"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	
	"github.com/byte-mug/fastnntp-backend2/storage"
	//"github.com/byte-mug/fastnntp-backend2/utils/minihash"
	//"sync"
	//"io"
	//"errors"
	
	"fmt"
	"path/filepath"
	"hash/fnv"
	"bytes"
	"time"
)

const tfnano = "20060102150405"

type RiLDB struct{
	MDB *leveldb.DB
	TDB *leveldb.DB
	RDB *leveldb.DB
}


type riLDBWriter struct{
	*RiLDB
	msgid []byte
	buf *bytes.Buffer
}

// Called for the first group/number-pair associated to the article
func(r *riLDBWriter) RiWrite(md *storage.Article_MD, rie *storage.RiElement) (err error) {
	msgid := r.msgid
	if !md.Expires.IsZero() {
		ha := fnv.New64a()
		ha.Write(msgid)
		tn := make([]byte,0,len(tfnano)+8)
		tn = md.Expires.AppendFormat(tn,tfnano)
		tn = ha.Sum(tn)
		err = r.TDB.Put(tn,msgid,nil)
		if err!=nil { return }
		r.RDB.Put(msgid,tn,nil)
		if err!=nil { return }
	}
	fmt.Fprintln(r.buf,string(rie.Group),rie.Num)
	return
}

// Called for the remaining group/number-pair associated to the article
func(r *riLDBWriter) RiWriteMore(md *storage.Article_MD, rie *storage.RiElement) (err error) {
	fmt.Fprintln(r.buf,string(rie.Group),rie.Num)
	return
}

// Called at after all group/number-pair have been associated.
func(r *riLDBWriter) RiCommit() (err error) {
	msgid, buf := r.msgid, r.buf
	
	err = r.MDB.Put(msgid,buf.Bytes(),nil)
	return
}

// Called for a sequence of group/number-pairs associated to the article
// This method may return <nil>!
func(r *RiLDB) RiBegin(msgid []byte) storage.RiWriter {
	return &riLDBWriter{RiLDB:r,msgid:msgid}
}


type relobj int
func(relobj) Release() {}

var relinst storage.Releaser = relobj(0)

// Performs a reverse index lookup: message-id to the first group/number pair.
func(r *RiLDB) RiLookup(msgid []byte,rie *storage.RiElement) (rel storage.Releaser,err error) {
	var rec []byte
	
	rec,err = r.MDB.Get(msgid,nil)
	
	if err!=nil { return }
	
	buf := bytes.NewBuffer(rec)
	
	var g string
	var n int64
	_,err = fmt.Fscanln(buf,&g,&n)
	
	if err!=nil { return }
	
	rie.Group = []byte(g)
	rie.Num = n
	
	rel = relinst
	
	return
}


type cursorLM struct{
	rie  *storage.RiElement
	mdb  *leveldb.DB
	buf  *bytes.Buffer
}

func (c *cursorLM) Release() {}
func (c *cursorLM) refill(key []byte) (error) {
	val,err := c.mdb.Get(key,nil)
	c.buf = bytes.NewBuffer(val)
	return err
}

func (c *cursorLM) Next() (ok bool) {
	if c.buf==nil { return }
	restart:
	if c.buf==nil { return }
	var g string
	var n int64
	p,_ := fmt.Fscanln(c.buf,&g,&n)
	switch p {
	case 0,2:
		ok = p==2
	case 1:
		goto restart
	}
	if ok {
		*c.rie = storage.RiElement{Group:[]byte(g),Num:n}
		return
	}
	return
}

// Performs a reverse index lookup: message-id to all first group/number pairs.
func(r *RiLDB) RiLookupAll(msgid []byte,rie *storage.RiElement) (rel storage.Cursor,err error) {
	cur := &cursorLM{rie,r.MDB,nil}
	err = cur.refill(msgid)
	if err==nil { rel = cur }
	return
}


type cursor struct{
	iter iterator.Iterator
	rih  *storage.RiHistory
	mdb  *leveldb.DB
	next bool
	buf  *bytes.Buffer
	key  []byte
	barrier []byte
}

func (c *cursor) Release() { c.iter.Release() }
func (c *cursor) refill() (ok bool) {
	if c.next {
		ok = c.iter.Next()
	} else {
		ok = c.iter.First()
		c.next = true
	}
	if ok {
		if string(c.iter.Key())>string(c.barrier) { return false }
		c.key = c.iter.Value()
		val,_ := c.mdb.Get(c.key,nil)
		c.buf = bytes.NewBuffer(val)
	}
	return
}
func (c *cursor) Next() (ok bool) {
	if c.buf==nil {
		if !c.refill() { return } /* End of it. */
	}
	restart:
	if c.buf==nil { return }
	var g string
	var n int64
	p,_ := fmt.Fscanln(c.buf,&g,&n)
	switch p {
	case 0,2:
		ok = p==2
	case 1:
		goto restart
	}
	if ok {
		*c.rih = storage.RiHistory{Group:[]byte(g),Num:n}
		return
	}
	if c.key!=nil {
		*c.rih = storage.RiHistory{MessageId:c.key}
		c.key = nil
		return true
	}
	if c.refill() { goto restart }
	return
}


// Query Expired articles. SHOULD return message-ids after their group/number counterparts.
func(r *RiLDB) RiQueryExpired(ow *time.Time, rih *storage.RiHistory) (cur storage.Cursor, err error) {
	lid := make([]byte,0,len(tfnano)+8)
	lid = ow.AppendFormat(lid,tfnano)
	lid = append(lid,0xff,0xff,0xff,0xff,0xff,0xff,0xff,0xff)
	
	iter := r.TDB.NewIterator(nil,nil)
	cur = &cursor{iter,rih,r.MDB,false,nil,nil,lid}
	return
}

// Expires an article using the message-id.
func(r *RiLDB) RiExpire(msgid []byte) (err error) {
	var tsid []byte
	
	tsid,err = r.RDB.Get(msgid,nil)
	if err!=nil { return }
	
	var err2,err3 error
	
	err = r.MDB.Delete(msgid,nil)
	err2 = r.TDB.Delete(tsid,nil)
	err3 = r.RDB.Delete(msgid,nil)
	
	if err==nil { err=err2 }
	if err==nil { err=err3 }
	
	return
}


func OpenSpoolRiLDB(spool string, o *opt.Options) (*RiLDB,error) {
	mdb,err := leveldb.OpenFile(filepath.Join(spool,"rildbm"), o)
	if err!=nil { return nil,err }
	tdb,err := leveldb.OpenFile(filepath.Join(spool,"rildbt"), o)
	if err!=nil { mdb.Close(); return nil,err }
	rdb,err := leveldb.OpenFile(filepath.Join(spool,"rildbr"), o)
	if err!=nil { mdb.Close(); return nil,err }
	return &RiLDB{
		MDB: mdb, // MessageID-DB
		TDB: tdb, // Time-DB
		RDB: rdb, // Reverse Time-DB
	},nil
}


func loader_rildb(cfg *storage.CfgMaster) (storage.RiMethod,error) {
	return OpenSpoolRiLDB(cfg.Spool,nil)
}


func init() {
	storage.RegisterRiLoader("rildb",loader_rildb)
}


