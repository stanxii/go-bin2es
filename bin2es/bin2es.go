package bin2es

import (
	"context"
	"sync"
	"time"
	"reflect"
	"strings"

	"github.com/juju/errors"
	"github.com/siddontang/go-mysql/canal"
	"github.com/siddontang/go-log/log"
	es7 "github.com/olivere/elastic/v7"
	"database/sql"
	_ "github.com/go-sql-driver/mysql"
)

type Empty 	   	 struct{}
type Event2Pipe  map[string]Bin2esConfig
type RefFuncMap  map[string]reflect.Value
type SQLPool     map[string]*sql.DB
type Set   	   	 map[string]Empty

type Bin2es struct {
	c      		*Config
	ctx    		context.Context
	cancel 		context.CancelFunc
	canal  		*canal.Canal
	wg     		sync.WaitGroup
	master 		*masterInfo
	esCli  		*MyES
	syncCh 		chan interface{}
	refFuncMap  RefFuncMap
	event2Pipe  Event2Pipe
	bin2esConf 	Bin2esConfig
	sqlPool     SQLPool
	tblFilter 	Set
	finish      chan bool
}

func NewBin2es(c *Config) (*Bin2es, error) {
	b := new(Bin2es)
	b.c = c
	b.finish = make(chan bool)
	b.syncCh = make(chan interface{}, 4096)
	b.sqlPool = make(SQLPool)
	b.tblFilter = make(Set)
	b.refFuncMap = make(RefFuncMap)
	b.event2Pipe = make(Event2Pipe)
	b.ctx, b.cancel = context.WithCancel(context.Background())

	var err error
	if b.master, err = loadMasterInfo(c.DataDir); err != nil {
		return nil, errors.Trace(err)
	}

	if err = b.initBin2esConf(); err != nil {
		return nil, errors.Trace(err)
	}

	if err = b.newCanal(); err != nil {
		return nil, errors.Trace(err)
	}

	if err = b.newES(); err != nil {
		return nil, errors.Trace(err)
	}

	return b, nil
}

func (b *Bin2es) newCanal() error {
	cfg := canal.NewDefaultConfig()
	cfg.Addr = b.c.Mysql.Addr
	cfg.User = b.c.Mysql.User
	cfg.Password = b.c.Mysql.Pwd
	cfg.Charset = b.c.Mysql.Charset
	cfg.Flavor = "mysql"

	cfg.ServerID = b.c.Mysql.ServerID
	cfg.Dump.ExecutionPath = "mysqldump"
	cfg.Dump.DiscardErr = false
	cfg.Dump.SkipMasterData = false

	var err error
	b.canal, err = canal.NewCanal(cfg)
	b.canal.SetEventHandler(&eventHandler{b})

	//init tblFilter
	for _, source := range b.c.Sources {
		schema := source.Schema
		for _, table := range source.Tables {
			key := schema + "." + table
			b.tblFilter[key] = Empty{}
		}
	}

	//prepare canal 
	for _, source := range b.c.Sources {
		b.canal.AddDumpTables(source.Schema, source.Tables...)
	}

	// We must use binlog full row image
	if err = b.canal.CheckBinlogRowImage("FULL"); err != nil {
		return errors.Trace(err)
	}

	return nil
}

func (b *Bin2es) isInTblFilter(key string) bool {
	_, ok := b.tblFilter[key]
	return ok
}

func (b *Bin2es) initBin2esConf() error {
	//read json config
	err := NewBin2esConfig("./config/binlog2es.json", &b.bin2esConf)
	if err != nil {
		log.Errorf("Failed to create ES Processor, err:%s", err)
		b.cancel()
		return errors.Trace(err)
	}

	//initialize event2Pipe
	set := make(map[string]Empty)
	for _, conf := range b.bin2esConf {
		schema := conf.Schema
		set[schema] = Empty{}
		for _, table := range conf.Tables {
			for _, action := range conf.Actions {
				key := strings.Join([]string{schema, table, action}, "_")
				b.event2Pipe[key] = append(b.event2Pipe[key], conf)
			}
		}
	}

	//initialize db connection
	for schema, _ := range set {
		user := b.c.Mysql.User
		pwd  := b.c.Mysql.Pwd
		addr := b.c.Mysql.Addr
		charset := b.c.Mysql.Charset
		dsn  := strings.Join([]string{user, ":", pwd, "@tcp(", addr, ")/", schema, "?charset=", charset}, "")
		
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			log.Errorf("sql Open error: %s", err)
			b.cancel()
			return errors.Trace(err)
		}
		if err = db.Ping(); err != nil {
			log.Errorf("db ping error: %s", err)
			b.cancel()
			return errors.Trace(err)
		}
		b.sqlPool[schema] = db

		log.Infof("connect to mysql successfully, dsn:%s", []string{dsn})
	}

	//initialize refFuncMap
	Value := reflect.ValueOf(reflectFunc{b})
    Type  := Value.Type()
    for i := 0; i < Value.NumMethod(); i++ {
    	key := Type.Method(i)
    	b.refFuncMap[key.Name] = Value.Method(i)
    }

	return nil
}

func (b *Bin2es) newES() error {
	var err error
	b.esCli = new(MyES)
	b.esCli.Ctx = b.Ctx()
	b.esCli.Client, err = es7.NewClient(
		es7.SetURL(b.c.Es.Nodes...),
		es7.SetHealthcheckInterval(10*time.Second),
		es7.SetGzip(true),
	)
	if err != nil {
		log.Errorf("Failed to create ES client, err:%s", err)
		b.cancel()
		return errors.Trace(err)
	}

	b.esCli.BulkService = b.esCli.Client.Bulk()

	log.Infof("connect to es successfully, addr:%s", b.c.Es.Nodes)

	return nil
}

func (b *Bin2es) Ctx() context.Context {
	return b.ctx
}

func (b *Bin2es) Run() error {
	defer log.Info("----- Canal closed -----")

	b.wg.Add(1)
	go b.syncES()

	pos := b.master.Position()
	if err := b.canal.RunFrom(pos); err != nil {
		log.Errorf("start canal err %v", err)
		b.cancel()
		return errors.Trace(err)
	}

	return nil
}

func (b *Bin2es) CloseDB() {
	defer log.Info("----- DB Closed -----")

	for _, db := range b.sqlPool {
		db.Close()
	}
}

func (b *Bin2es) Close() {
	defer log.Info("----- Bin2es Closed -----")

	log.Info("closing bin2es")

	b.cancel()

	b.canal.Close()

	b.master.Close()

	<-b.finish

	b.esCli.Close()

	b.CloseDB()

	//消耗完剩余syncCh里的消息, 不然会有一定概率阻塞Canal组件的关闭
	for {
        select {
        case <-b.syncCh:
        default:
        	goto END
        }
    }

END:
	log.Infof("close sync channel")
	
	close(b.syncCh)

	b.wg.Wait()
}