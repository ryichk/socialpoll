package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/nsqio/go-nsq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var fatalErr error

func fatal(e error) {
	fmt.Println(e)
	flag.PrintDefaults()
	fatalErr = e
}

const updateDuration = 1 + time.Second

func main() {
	defer func() {
		if fatalErr != nil {
			// 終了コード1を返してプログラム全体を終了
			os.Exit(1)
		}
	}()

	log.Println("データベースに接続します...")
	credential := options.Credential{
		Username: "root",
		Password: "password",
	}
	clientOpts := options.Client().ApplyURI("mongodb://mongo:27017").SetAuth(credential)
	db, err := mongo.Connect(context.TODO(), clientOpts)
	if err != nil {
		log.Fatalln("データベース接続に失敗しました:", err)
		return
	}

	defer func() {
		log.Println("データベース接続を閉じます...")
		if err := db.Disconnect(context.TODO()); err != nil {
			log.Fatalln("データベース接続を閉じることに失敗しました:", err)
		}
		log.Println("データベース接続が閉じられました")
	}()

	pollData := db.Database("ballots").Collection("polls")

	var countsLock sync.Mutex
	var counts map[string]int
	log.Println("NSQに接続します...")
	q, err := nsq.NewConsumer("votes", "counter", nsq.NewConfig())
	if err != nil {
		fatal(err)
		return
	}
	// votes上でメッセージが受信されるたびに呼び出される
	q.AddHandler(nsq.HandlerFunc(func(m *nsq.Message) error {
		countsLock.Lock()
		defer countsLock.Unlock()
		if counts == nil {
			counts = make(map[string]int)
		}
		vote := string(m.Body)
		counts[vote]++
		return nil
	}))
	// NSQのサービスに接続
	if err := q.ConnectToNSQLookupd("nsqlookupd:4161"); err != nil {
		fatal(err)
		return
	}

	log.Println("NSQ上での投票を待機します...")
	var updater *time.Timer
	updater = time.AfterFunc(updateDuration, func() {
		countsLock.Lock()
		defer countsLock.Unlock()
		if len(counts) == 0 {
			log.Println("新しい投票はありません。データベースの更新をスキップします")
		} else {
			log.Println("データベースを更新します...")
			log.Println(counts)
			ok := true
			for option, count := range counts {
				filter := bson.M{"options": bson.M{"$in": []string{option}}}
				update := bson.M{"$inc": bson.M{"results." + option: count}}
				if _, err := pollData.UpdateMany(context.TODO(), filter, update); err != nil {
					log.Println("更新に失敗しました:", err)
					ok = false
					continue
				}
				counts[option] = 0
			}
			if ok {
				log.Println("データベースの更新が完了しました")
				counts = nil // 得票数をリセット
			}
		}
		updater.Reset(updateDuration)
	})

	termChan := make(chan os.Signal, 1)
	signal.Notify(termChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case <-termChan:
			updater.Stop()
			q.Stop()
		case <-q.StopChan:
			// 完了しました
			return
		}
	}
}
