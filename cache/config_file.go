/*
 * Tencent is pleased to support the open source community by making Polaris available.
 *
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 *
 * Licensed under the BSD 3-Clause License (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://opensource.org/licenses/BSD-3-Clause
 *
 * Unless required by applicable law or agreed to in writing, software distributed
 * under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
 * CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 */

package cache

import (
	"github.com/polarismesh/polaris-server/common/log"
	"github.com/polarismesh/polaris-server/common/model"
	"github.com/polarismesh/polaris-server/store"
	"go.uber.org/zap"
	"math/rand"
	"sync"
	"time"
)

const (
	BaseExpireTimeAfterWrite = 60 * 60 // expire after 1 hour
	FileIdSeparator          = "+"
)

var (
	putCnt    = 0
	loadCnt   = 0
	getCnt    = 0
	removeCnt = 0
	expireCnt = 0
)

// FileCache 文件缓存，使用 loading cache 懒加载策略。同时使用写入一段时间后失效策略
type FileCache struct {
	storage store.Store
	//fileId -> Entry
	files *sync.Map
	//fileId -> lock
	fileLoadLocks *sync.Map
}

// Entry 缓存实体对象
type Entry struct {
	Content string
	Md5     string
	Version uint64
	//创建的时候，设置过期时间
	ExpireTime time.Time
	//标识是否是空缓存
	Empty bool
}

func NewFileCache(storage store.Store) *FileCache {
	cache := &FileCache{
		storage:       storage,
		files:         new(sync.Map),
		fileLoadLocks: new(sync.Map),
	}

	cache.startClearExpireEntryTask()
	cache.startLogStatusTask()

	return cache
}

// Put 写入缓存对象
func (fc *FileCache) Put(file *model.ConfigFileRelease) {
	putCnt++
	fileId := GenFileId(file.Namespace, file.Group, file.FileName)

	storedEntry, ok := fc.Get(file.Namespace, file.Group, file.FileName)
	//幂等判断，只能存入版本号更大的
	if !ok || storedEntry.Empty || file.Version > storedEntry.Version {
		entry := newEntry(file.Content, file.Md5, file.Version)
		fc.files.Store(fileId, entry)
	}
}

// Get 一般用于内部服务调用，所以不计入 metrics
func (fc *FileCache) Get(namespace, group, fileName string) (*Entry, bool) {
	fileId := GenFileId(namespace, group, fileName)
	storedEntry, ok := fc.files.Load(fileId)
	if ok {
		entry := storedEntry.(*Entry)
		return entry, true
	}
	return nil, false
}

// GetOrLoadIfAbsent 获取缓存，如果缓存没命中则会从数据库中加载，如果数据库里获取不到数据，则会缓存一个空对象防止缓存一直被击穿
func (fc *FileCache) GetOrLoadIfAbsent(namespace, group, fileName string) (*Entry, error) {
	getCnt++

	fileId := GenFileId(namespace, group, fileName)
	storedEntry, ok := fc.files.Load(fileId)
	if ok {
		entry := storedEntry.(*Entry)
		return entry, nil
	}

	//为了避免在大并发量的情况下，数据库被压垮，所以增加锁。同时为了提高性能，减小锁粒度
	lockObj, _ := fc.fileLoadLocks.LoadOrStore(fileId, new(sync.Mutex))
	loadLock := lockObj.(*sync.Mutex)
	loadLock.Lock()
	defer loadLock.Unlock()

	//double check
	storedEntry, ok = fc.files.Load(fileId)
	if ok {
		entry := storedEntry.(*Entry)
		return entry, nil
	}

	loadCnt++

	file, err := fc.storage.GetConfigFileRelease(nil, namespace, group, fileName)
	if err != nil {
		log.GetConfigLogger().Error("[Config][Cache] load config file release error.",
			zap.String("namespace", namespace),
			zap.String("group", group),
			zap.String("fileName", fileName),
			zap.Error(err))
		return nil, err
	}

	if file != nil {
		entry := newEntry(file.Content, file.Md5, file.Version)
		fc.files.Store(fileId, entry)
		return entry, nil
	}

	//为了避免对象不存在时，一直击穿数据库，所以缓存空对象
	emptyEntry := &Entry{
		Content:    "",
		ExpireTime: getExpireTime(),
		Empty:      true,
	}
	fc.files.Store(fileId, emptyEntry)

	return emptyEntry, nil
}

// Remove 删除缓存对象
func (fc *FileCache) Remove(namespace, group, fileName string) {
	removeCnt++
	fileId := GenFileId(namespace, group, fileName)
	fc.files.Delete(fileId)
}

func newEntry(content, md5 string, version uint64) *Entry {
	return &Entry{
		Content:    content,
		Md5:        md5,
		Version:    version,
		ExpireTime: getExpireTime(),
		Empty:      false,
	}
}

// GenFileId 生成文件对象 Id
func GenFileId(namespace, group, fileName string) string {
	return namespace + FileIdSeparator + group + FileIdSeparator + fileName
}

//缓存过期时间，为了避免集中失效，加上随机数。[60 ~ 70]分钟内随机失效
func getExpireTime() time.Time {
	randTime := rand.Intn(10*60) + BaseExpireTimeAfterWrite
	return time.Now().Add(time.Duration(randTime) * time.Second)
}

//定时清理过期的缓存
func (fc *FileCache) startClearExpireEntryTask() {
	t := time.NewTicker(time.Minute)
	go func() {
		for {
			select {
			case <-t.C:
				curExpiredFileCnt := 0
				fc.files.Range(func(fileId, entry interface{}) bool {
					if time.Now().After(entry.(*Entry).ExpireTime) {
						fc.files.Delete(fileId)
						curExpiredFileCnt++
					}
					return true
				})

				if curExpiredFileCnt > 0 {
					log.GetConfigLogger().Info("[Config][Cache] clear expired file cache.", zap.Int("count", curExpiredFileCnt))
				}

				expireCnt += curExpiredFileCnt
			}
		}
	}()
}

//print cache status at fix rate
func (fc *FileCache) startLogStatusTask() {
	t := time.NewTicker(time.Minute)
	go func() {
		for {
			select {
			case <-t.C:
				log.GetConfigLogger().Info("[Config][Cache] cache status:", zap.Int("getCnt", getCnt),
					zap.Int("putCnt", putCnt),
					zap.Int("loadCnt", loadCnt),
					zap.Int("removeCnt", removeCnt),
					zap.Int("expireCnt", expireCnt))
			}
		}
	}()
}
