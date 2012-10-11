/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package mgo

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	. "launchpad.net/hockeypuck"
	"bitbucket.org/cmars/go.crypto/openpgp/armor"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
)

const UUID_LEN = 43  // log(2**256, 64) = 42.666...

func NewUuid() (string, error) {
	buf := bytes.NewBuffer([]byte{})
	enc := base64.NewEncoder(base64.StdEncoding, buf)
	n, err := io.CopyN(enc, rand.Reader, UUID_LEN)
	if err != nil {
		return "", err
	}
	if n < UUID_LEN {
		return "", errors.New("Failed to generate UUID")
	}
	return string(buf.Bytes()), nil
}

type MgoWorker struct {
	WorkerBase
	session *mgo.Session
	c *mgo.Collection
}

func (mw *MgoWorker) Init(connect string) (err error) {
	mw.WorkerBase.Init()
	mw.L.Println("Connecting to mongodb:", connect)
	mw.session, err = mgo.Dial(connect)
	if err != nil {
		mw.L.Println("Connection failed:", err)
		return
	}
	mw.session.SetMode(mgo.Strong, true)
	// Conservative on writes
	mw.session.EnsureSafe(&mgo.Safe{
		W: 1,
		FSync: true })
	mw.c = mw.session.DB("hockeypuck").C("keys")
	fpIndex := mgo.Index{
		Key: []string{ "fingerprint" },
		Unique: true,
		DropDups: false,
		Background: false,
		Sparse: false }
	err = mw.c.EnsureIndex(fpIndex)
	if err != nil {
		mw.L.Println("Ensure index failed:", err)
		return
	}
	kwIndex := mgo.Index{
		Key: []string{ "identities.keywords" },
		Unique: false,
		DropDups: false,
		Background: true,
		Sparse: false }
	err = mw.c.EnsureIndex(kwIndex)
	if err != nil {
		mw.L.Println("Ensure index failed:", err)
		return
	}
	return
}

func (mw *MgoWorker) LookupKeys(search string, limit int) (keys []*PubKey, err error) {
	q := mw.c.Find(bson.M{ "identities.keywords": search })
	n, err := q.Count()
	if n > limit {
		return keys, TooManyResponses
	}
	pubKey := new(PubKey)
	iter := q.Iter()
	for iter.Next(pubKey) {
		keys = append(keys, pubKey)
	}
	err = iter.Err()
	return
}

func (mw *MgoWorker) LookupKey(keyid string) (*PubKey, error) {
	keyid = strings.ToLower(keyid)
	raw, err := hex.DecodeString(keyid)
	if err != nil {
		return nil, InvalidKeyId
	}
	var q *mgo.Query
	switch len(raw) {
	case 4:
		q = mw.c.Find(bson.M{ "shortid": raw })
	case 8:
		q = mw.c.Find(bson.M{ "keyid": raw })
	case 20:
		q = mw.c.Find(bson.M{ "fingerprint": keyid })
	default:
		return nil, InvalidKeyId
	}
	key := new(PubKey)
	err = q.One(key)
	if err == mgo.ErrNotFound {
		return nil, KeyNotFound
	} else if err != nil {
		return nil, err
	}
	return key, nil
}

func (mw *MgoWorker) AddKey(armoredKey string) error {
	mw.L.Print("AddKey(...)")
	// Check and decode the armor
	armorBlock, err := armor.Decode(bytes.NewBufferString(armoredKey))
	if err != nil {
		return err
	}
	return mw.LoadKeys(armorBlock.Body)
}

func (mw *MgoWorker) LoadKeys(r io.Reader) (err error) {
	keyChan, errChan := ReadKeys(r)
	for {
		select {
		case key, moreKeys :=<-keyChan:
			if key != nil {
				lastKey, err := mw.LookupKey(key.Fingerprint)
				if err == nil && lastKey != nil {
					mw.L.Print("Merge/Update:", key.Fingerprint)
					MergeKey(lastKey, key)
					err = mw.c.Update(bson.M{ "fingerprint": key.Fingerprint }, lastKey)
				} else if err == KeyNotFound {
					mw.L.Print("Insert:", key.Fingerprint)
					err = mw.c.Insert(key)
				}
				if err != nil {
					return err
				}
			}
			if !moreKeys {
				return err
			}
		case err :=<-errChan:
			return err
		}
	}
	panic("unreachable")
}
