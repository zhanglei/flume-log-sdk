package consumer

import (
	"container/list"
	"encoding/json"
	"flume-log-sdk/config"
	"flume-log-sdk/consumer/client"
	"flume-log-sdk/consumer/pool"
	"flume-log-sdk/rpc/flume"
	"fmt"
	"github.com/blackbeans/redigo/redis"
	"log"
	"math/rand"
	"sync/atomic"
	"time"
)

type counter struct {
	lastSuccValue int64

	currSuccValue int64

	lastFailValue int64

	currFailValue int64
}

// 用于向flume中作为sink 通过thrift客户端写入日志

type SourceServer struct {
	redisPool       map[string][]*redis.Pool
	flumeClientPool *list.List
	isStop          bool
	monitorCount    counter
	business        string
	batchSize       int
	sendbuff        int
}

func newSourceServer(business string, redisPool map[string][]*redis.Pool, flumePool *list.List) (server *SourceServer) {
	batchSize := 300
	sendbuff := 10
	sourceServer := &SourceServer{business: business, redisPool: redisPool,
		flumeClientPool: flumePool, batchSize: batchSize, sendbuff: sendbuff}
	return sourceServer
}

func (self *SourceServer) monitor() (succ, fail int64) {
	currSucc := self.monitorCount.currSuccValue
	currFail := self.monitorCount.currFailValue
	succ = (currSucc - self.monitorCount.lastSuccValue)
	fail = (currFail - self.monitorCount.lastFailValue)
	self.monitorCount.lastSuccValue = currSucc
	self.monitorCount.lastFailValue = currFail
	return
}

//启动pop
func (self *SourceServer) start() {

	self.isStop = false

	var count = 0
	for k, v := range self.redisPool {

		log.Println("LOG_SOURCE|REDIS|" + k + "|START")
		for _, pool := range v {
			count++

			go func(queuename string, pool *redis.Pool) {

				//创建chan ,buffer 为10
				// sendbuff := make(chan []*flume.ThriftFlumeEvent, self.sendbuff)
				sendbuff := make(chan []*flume.ThriftFlumeEvent, 10)
				defer close(sendbuff)
				//启动20个go程从channel获取
				for i := 0; i < 10; i++ {
					go func(ch chan []*flume.ThriftFlumeEvent) {
						for !self.isStop {
							events := <-ch
							self.innerSend(events)
						}
					}(sendbuff)
				}

				//批量收集数据
				conn := pool.Get()
				defer pool.Release(conn)
				pack := make([]*flume.ThriftFlumeEvent, 0, self.batchSize)
				for !self.isStop {

					reply, err := conn.Do("LPOP", queuename)
					if nil != err || nil == reply {
						if nil != err {
							log.Printf("LPOP|FAIL|%T", err)
							conn.Close()
							conn = pool.Get()
						} else {
							time.Sleep(100 * time.Millisecond)
						}

						continue
					}

					resp := reply.([]byte)
					var cmd config.Command
					err = json.Unmarshal(resp, &cmd)

					if nil != err {
						log.Printf("command unmarshal fail ! %T | error:%s\n", resp, err.Error())
						continue
					}
					//
					momoid := cmd.Params["momoid"].(string)

					businessName := cmd.Params["businessName"].(string)

					action := cmd.Params["type"].(string)

					bodyContent := cmd.Params["body"]

					//将businessName 加入到body中
					bodyMap := bodyContent.(map[string]interface{})
					bodyMap["business_type"] = businessName

					body, err := json.Marshal(bodyContent)
					if nil != err {
						log.Printf("marshal log body fail %s", err.Error())
						continue
					}

					//拼Body
					flumeBody := fmt.Sprintf("%s\t%s\t%s", momoid, action, string(body))

					event := client.NewFlumeEvent(businessName, action, []byte(flumeBody))
					//如果总数大于batchsize则提交
					if len(pack) < self.batchSize {
						//批量提交
						pack = append(pack, event)
						continue
					}
					sendbuff <- pack[:len(pack)]
					pack = make([]*flume.ThriftFlumeEvent, 0, self.batchSize)
				}
			}(k, pool)
		}
	}
	log.Printf("LOG_SOURCE|SOURCE SERVER [%s]|STARTED\n", self.business)
}

func (self *SourceServer) innerSend(events []*flume.ThriftFlumeEvent) {

	for i := 0; i < 3; i++ {
		pool := self.getFlumeClientPool()
		flumeclient, err := pool.Get(5 * time.Second)
		if nil != err || nil == flumeclient {
			log.Printf("LOG_SOURCE|GET FLUMECLIENT|FAIL|%s|%s|TRY:%d\n", self.business, err, i)
			continue
		}

		err = flumeclient.AppendBatch(events)
		defer func() {
			if err := recover(); nil != err {
				//回收这个坏的连接
				pool.ReleaseBroken(flumeclient)
			} else {
				pool.Release(flumeclient)
			}
		}()

		if nil != err {
			atomic.AddInt64(&self.monitorCount.currFailValue, int64(1*self.batchSize))
			log.Printf("LOG_SOURCE|SEND FLUME|FAIL|%s|%s|TRY:%d\n", self.business, err.Error(), i)

		} else {
			atomic.AddInt64(&self.monitorCount.currSuccValue, int64(1*self.batchSize))
			if rand.Int()%10000 == 0 {
				log.Printf("trace|send 2 flume succ|%s|%d\n", flumeclient.HostPort(), len(events))
			}
			break
		}

	}
}

//仅供测试使用推送数据
func (self *SourceServer) testPushLog(queuename, logger string) {

	for _, v := range self.redisPool {
		for _, pool := range v {
			conn := pool.Get()
			defer pool.Release(conn)

			reply, err := conn.Do("RPUSH", queuename, logger)
			log.Printf("testPushLog|%d|err:%s", reply, err)
			break

		}
	}

}

func (self *SourceServer) stop() {
	self.isStop = true
	time.Sleep(5 * time.Second)

	//遍历所有的flumeclientlink，将当前Business从该链表中移除
	for v := self.flumeClientPool.Back(); nil != v; v = v.Prev() {
		v.Value.(*pool.FlumePoolLink).DetachBusiness(self.business)
	}
	log.Printf("LOG_SOURCE|SOURCE SERVER|[%s]|STOPPED\n", self.business)
}

func (self *SourceServer) getFlumeClientPool() *pool.FlumeClientPool {

	//采用轮训算法
	e := self.flumeClientPool.Back()
	self.flumeClientPool.MoveToFront(e)
	return e.Value.(*pool.FlumePoolLink).FlumePool

}