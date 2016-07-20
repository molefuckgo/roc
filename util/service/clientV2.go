// Copyright 2014 The roc Author. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.


package rocserv

import (
	"fmt"
	"time"
	"strconv"
	"strings"
	"sort"
	"sync"
	"encoding/json"


	"github.com/shawnfeng/consistent"
    etcd "github.com/coreos/etcd/client"

	"github.com/shawnfeng/sutil/slog"
	"github.com/shawnfeng/sutil/stime"

	"golang.org/x/net/context"
)


type servCopyStr struct {
	servId int
	reg string
	manual string
}

type servCopyData struct {
	servId int
	reg *RegData
	manual *ManualData
}


type servCopyCollect map[int]*servCopyData

func (m servCopyCollect) String() string {
	var copys []string
	for idx, v := range m {
		if v == nil {
			copys = append(copys, fmt.Sprintf("%d[nil]", idx))
			continue
		}

		reg := "nil"
		manual := "nil"

		if v.reg != nil {
			reg = v.reg.String()
		}
		if v.manual != nil {
			manual = v.manual.String()
		}

		copys = append(copys, fmt.Sprintf("%d[%s]%s", idx, reg, manual))
	}


	return strings.Join(copys, ";")
}

type ClientEtcdV2 struct {
	confEtcd configEtcd
	servKey string
	servPath string
	// 使用的注册器位置，不同版本会注册到不同版本的dist目录
	// 但是会保持多版本的兼容，客户端优先使用最新版本的
	distLoc  string

	etcdClient etcd.KeysAPI

	// 缓存地址列表，按照service id 降序的顺序存储
	// 按照processor 进行分组

	muServlist sync.Mutex
	//servList map[string][]*ServInfo
	servCopy servCopyCollect
	servHash  *consistent.Consistent

}


func checkDistVersion(client etcd.KeysAPI, prefloc, servlocation string) string {
	fun := "checkDistVersion -->"

	path := fmt.Sprintf("%s/%s/%s", prefloc, BASE_LOC_DIST_V2, servlocation)

    r, err := client.Get(context.Background(), path, &etcd.GetOptions{Recursive: true, Sort: false})
	if err == nil {
		slog.Infof("%s check dist v2 ok path:%s", fun, path)
		for _, n := range r.Node.Nodes {
			for _, nc := range n.Nodes {
				if nc.Key == n.Key+"/"+BASE_LOC_REG_SERV && len(nc.Value) > 0 {
					return BASE_LOC_DIST_V2
				}
			}
		}

	}

	slog.Warnf("%s check dist v2 path:%s err:%s", fun, path, err)


	path = fmt.Sprintf("%s/%s/%s", prefloc, BASE_LOC_DIST, servlocation)

    _, err = client.Get(context.Background(), path, &etcd.GetOptions{Recursive: true, Sort: false})
	if err == nil {
		slog.Infof("%s check dist v1 ok path:%s", fun, path)
		return BASE_LOC_DIST
	}

	slog.Warnf("%s user v2 if check dist v1 path:%s err:%s", fun, path, err)

	return BASE_LOC_DIST_V2
}

func NewClientEtcdV2(confEtcd configEtcd, servlocation string) (*ClientEtcdV2, error) {
	//fun := "NewClientEtcdV2 -->"

	cfg := etcd.Config{
		Endpoints: confEtcd.etcdAddrs,
		Transport: etcd.DefaultTransport,
	}

	c, err := etcd.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create etchd client cfg error")
	}

    client := etcd.NewKeysAPI(c)
	if client == nil {
		return nil, fmt.Errorf("create etchd api error")
	}

	distloc := checkDistVersion(client, confEtcd.useBaseloc, servlocation)

	cli := &ClientEtcdV2 {
		confEtcd: confEtcd,
		servKey: servlocation,
		distLoc: distloc,
		servPath: fmt.Sprintf("%s/%s/%s", confEtcd.useBaseloc, distloc, servlocation),

		etcdClient: client,
	}

	cli.watch()

	return cli, nil

}

func (m *ClientEtcdV2) startWatch(chg chan *etcd.Response) {
	fun := "ClientEtcdV2.startWatch -->"

	path := m.servPath

	for i := 0; ; i++ {
		r, err := m.etcdClient.Get(context.Background(), path, &etcd.GetOptions{Recursive: true, Sort: false})
		if err != nil {
			slog.Errorf("%s get path:%s err:%s", fun, path, err)
			close(chg)
			return
		} else {
			chg <- r
		}

		sresp, _ := json.Marshal(r)
		slog.Infof("%s init get action:%s nodes:%d index:%d servPath:%s resp:%s", fun, r.Action, len(r.Node.Nodes), r.Index, path, sresp)

		// 每次循环都设置下，测试发现放外边不好使
		wop := &etcd.WatcherOptions{
			Recursive: true,
			AfterIndex: r.Index,
		}
		watcher := m.etcdClient.Watcher(path, wop)
		if watcher == nil {
			slog.Errorf("%s new watcher path:%s", fun, path)
			return
		}

		resp, err := watcher.Next(context.Background())
		// etcd 关闭时候会返回
		if err != nil {
			slog.Errorf("%s watch path:%s err:%s", fun, path, err)
			close(chg)
			return
		} else {
			slog.Infof("%s next get idx:%d action:%s nodes:%d index:%d after:%d servPath:%s", fun, i, resp.Action, len(resp.Node.Nodes), resp.Index, wop.AfterIndex, path)
			// 测试发现next获取到的返回，index，重新获取总有问题，触发两次，不确定，为什么？为什么？
			// 所以这里每次next前使用的afterindex都重新get了
		}
	}

}

func (m *ClientEtcdV2) watch() {
	fun := "ClientEtcdV2.watch -->"

	backoff := stime.NewBackOffCtrl(time.Millisecond * 10, time.Second * 5)

	var chg chan *etcd.Response

	go func() {
		slog.Infof("%s start watch:%s", fun, m.servPath)
		for {
			//slog.Infof("%s loop watch", fun)
			if chg == nil {
				slog.Infof("%s loop watch new receiver:%s", fun, m.servPath)
				chg = make(chan *etcd.Response)
				go m.startWatch(chg)
			}

			r, ok := <-chg
			if !ok {
				slog.Errorf("%s chg info nil:%s", fun, m.servPath)
				chg = nil
				backoff.BackOff()
			} else {
				slog.Infof("%s update v:%s serv:%s", fun, r.Node.Key, m.servPath)
				m.parseResponse(r)
				backoff.Reset()
			}

		}
	}()
}


func (m *ClientEtcdV2) parseResponse(r *etcd.Response) {
	fun := "ClientEtcdV2.parseResponse -->"
	/*
    r, err := m.etcdClient.Get(context.Background(), m.servPath, &etcd.GetOptions{Recursive: true, Sort: false})
	if err != nil {
		slog.Errorf("%s get err:%s", fun, err)
	}

	if r == nil {
		slog.Errorf("%s nil", fun)
		return
	}
    */

	if !r.Node.Dir {
		slog.Errorf("%s not dir %s", fun, r.Node.Key)
		return
	}

	if m.distLoc == BASE_LOC_DIST {
		m.parseResponseV1(r)
	} else if m.distLoc == BASE_LOC_DIST_V2 {
		m.parseResponseV2(r)
	} else {
		slog.Errorf("%s not support:%s dir:%s", fun, m.distLoc, r.Node.Key)
	}

}

func (m *ClientEtcdV2) parseResponseV2(r *etcd.Response) {
	fun := "ClientEtcdV2.parseResponseV2 -->"

	idServ := make(map[int]*servCopyStr)
	ids := make([]int, 0)
	for _, n := range r.Node.Nodes {
		if !n.Dir {
			slog.Errorf("%s not dir %s", fun, n.Key)
			return
		}

		sid := n.Key[len(r.Node.Key)+1:]
		id, err := strconv.Atoi(sid)
		if err != nil || id < 0 {
			slog.Errorf("%s sid error key:%s", fun, n.Key)
			continue
		}
		ids = append(ids, id)


		var reg, manual string
		for _, nc := range n.Nodes {
			slog.Infof("%s dist key:%s value:%s", fun, nc.Key, nc.Value)

			if nc.Key == n.Key+"/"+BASE_LOC_REG_SERV {
				reg = nc.Value
			} else if nc.Key == n.Key+"/"+BASE_LOC_REG_MANUAL {
				manual = nc.Value
			}
		}
		idServ[id] = &servCopyStr {
			servId: id,
			reg: reg,
			manual: manual,
		}


	}
	sort.Ints(ids)

	slog.Infof("%s chg action:%s nodes:%d index:%d servPath:%s len:%d", fun, r.Action, len(r.Node.Nodes), r.Index, m.servPath, len(ids))
	if len(ids) == 0 {
		slog.Errorf("%s not found service path:%s please check deploy", fun, m.servPath)
	}

	//slog.Infof("%s chg servpath:%s ids:%v", fun, r.Action, len(r.Node.Nodes), r.EtcdIndex, r.RaftIndex, r.RaftTerm, m.servPath, ids)

	servCopy := make(servCopyCollect)
	//for _, s := range vs {
	for _, i := range ids {
		is := idServ[i]
		if is == nil {
			slog.Warnf("%s serv not found idx:%d servpath:%s", fun, i, m.servPath)
			continue
		}

		var regd RegData
		if len(is.reg) > 0 {
			err := json.Unmarshal([]byte(is.reg), &regd)
			if err != nil {
				slog.Errorf("%s servpath:%s sid:%d json:%s error:%s", fun, m.servPath, i, is.reg, err)
			}
			if len(regd.Servs) == 0 {
				slog.Errorf("%s not found copy path:%s sid:%d info:%s please check deploy", fun, m.servPath, i, is.reg)
			}
		}


		var manual ManualData
		if len(is.manual) > 0 {
			err := json.Unmarshal([]byte(is.manual), &manual)
			if err != nil {
				slog.Errorf("%s servpath:%s json:%s error:%s", fun, m.servPath, is.manual, err)
			}
		}


		servCopy[i] = &servCopyData {
			servId: i,
			reg: &regd,
			manual: &manual,
		}


	}

	m.upServlist(servCopy)
}

func (m *ClientEtcdV2) parseResponseV1(r *etcd.Response) {
	fun := "ClientEtcdV2.parseResponseV1 -->"

	idServ := make(map[int]string)
	ids := make([]int, 0)
	for _, n := range r.Node.Nodes {
		sid := n.Key[len(r.Node.Key)+1:]
		id, err := strconv.Atoi(sid)
		if err != nil || id < 0 {
			slog.Errorf("%s sid error key:%s", fun, n.Key)
		} else {
			slog.Infof("%s dist key:%s value:%s", fun, n.Key, n.Value)
			ids = append(ids, id)
			idServ[id] = n.Value
		}
	}
	sort.Ints(ids)

	slog.Infof("%s chg action:%s nodes:%d index:%d servPath:%s len:%d", fun, r.Action, len(r.Node.Nodes), r.Index, m.servPath, len(ids))
	if len(ids) == 0 {
		slog.Errorf("%s not found service path:%s please check deploy", fun, m.servPath)
	}

	//slog.Infof("%s chg servpath:%s ids:%v", fun, r.Action, len(r.Node.Nodes), r.EtcdIndex, r.RaftIndex, r.RaftTerm, m.servPath, ids)

	servCopy := make(servCopyCollect)
	//for _, s := range vs {
	for _, i := range ids {
		s := idServ[i]


		var servs map[string]*ServInfo
		err := json.Unmarshal([]byte(s), &servs)
		if err != nil {
			slog.Errorf("%s servpath:%s json:%s error:%s", fun, m.servPath, s, err)
		}

		if len(servs) == 0 {
			slog.Errorf("%s not found copy path:%s info:%s please check deploy", fun, m.servPath, s)
		}

		servCopy[i] = &servCopyData {
			servId: i,
			reg: &RegData{
				Servs: servs,
			},
		}

	}

	m.upServlist(servCopy)
}

func (m *ClientEtcdV2) upServlist(scopy map[int]*servCopyData) {
	fun := "ClientEtcdV2.upServlist -->"

	var slist []string
	for sid, c := range scopy {
		if c == nil {
			slog.Infof("%s not found copy path:%s sid:%d", fun, m.servPath, sid)
			continue
		}

		if c.reg == nil {
			slog.Infof("%s not found regdata path:%s sid:%d", fun, m.servPath, sid)
			continue
		}

		if len(c.reg.Servs) == 0 {
			slog.Infof("%s not found servs path:%s sid:%d", fun, m.servPath, sid)
			continue
		}

		var weight int
		var disable bool
		if c.manual != nil && c.manual.Ctrl != nil {
			weight = c.manual.Ctrl.Weight
			disable = c.manual.Ctrl.Disable
		}
		if weight == 0 {
			weight = 100
		}

		if disable {
			slog.Infof("%s disable path:%s sid:%d", fun, m.servPath, sid)
			continue
		}

		for i := 0; i < weight; i++ {
			slist = append(slist, fmt.Sprintf("%d-%d", sid, i))
		}
	}


	shash := consistent.NewWithElts(slist)
	slog.Infof("%s path:%s serv:%d", fun, m.servPath, len(slist))

	m.muServlist.Lock()
	defer m.muServlist.Unlock()

	m.servHash = shash
	m.servCopy = scopy

	slog.Infof("%s serv:%s servcopy:%s", fun, m.servPath, m.servCopy)
}

func (m *ClientEtcdV2) GetServAddr(processor, key string) *ServInfo {
	fun := "ClientEtcdV2.GetServAddr -->"
	m.muServlist.Lock()
	defer m.muServlist.Unlock()

	if m.servHash == nil {
		slog.Errorf("%s empty serv path:%s hash circle processor:%s key:%s", fun, m.servPath, processor, key)
		return nil
	}

	s, err := m.servHash.Get(key)
	if err != nil {
		slog.Errorf("%s get serv path:%s processor:%s key:%s err:%s", fun, m.servPath, processor, key, err)
		return nil
	}

	idx := strings.Index(s, "-")
	if idx == -1 {
		slog.Fatalf("%s servid path:%s processor:%s key:%s sid:%s", fun, m.servPath, processor, key, s)
		return nil
	}

	sid, err := strconv.Atoi(s[:idx])
	if err != nil || sid < 0 {
		slog.Fatalf("%s servid path:%s processor:%s key:%s sid:%s id:%d err:%s", fun, m.servPath, processor, key, s, sid, err)
		return nil
	}
	return m.getServAddrWithServid(sid, processor, key)
}


func (m *ClientEtcdV2) getServAddrWithServid(servid int, processor, key string) *ServInfo {
	if c := m.servCopy[servid]; c != nil {
		if c.reg != nil {
			if c.manual != nil && c.manual.Ctrl != nil && c.manual.Ctrl.Disable {
				return nil
			}
			if p := c.reg.Servs[processor]; p != nil {
				return p
			}
		}
	}

	return nil
}


func (m *ClientEtcdV2) GetServAddrWithServid(servid int, processor, key string) *ServInfo {
	m.muServlist.Lock()
	defer m.muServlist.Unlock()

	return m.getServAddrWithServid(servid, processor, key)
}

/*
func (m *ClientEtcdV2) GetAllServAddr() map[string][]*ServInfo {
	m.muServlist.Lock()
	defer m.muServlist.Unlock()

	rv := make(map[string][]*ServInfo)
	for k, v := range m.servList {
		n := make([]*ServInfo, len(v))
		copy(n, v)
		rv[k] = n
	}


	return rv
}
*/


func (m *ClientEtcdV2) ServKey() string {
	return m.servKey
}


func (m *ClientEtcdV2) ServPath() string {
	return m.servPath
}
