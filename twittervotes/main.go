package main

import (
	"context"
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

func main() {
	var stoplock sync.Mutex
	stop := false
	stopChan := make(chan struct{}, 1) // プロセスを終了させたいということを示すために使われる
	signalChan := make(chan os.Signal, 1)
	go func() {
		// signalChanへのシグナルの着信を待機してブロック
		// <-はチャネルからの読み込みを試みる演算子
		<-signalChan
		stoplock.Lock()
		// シグナルを受診したら、stopにtrueをセットし接続を閉じる
		stop = true
		stoplock.Unlock()
		log.Println("停止します...")
		stopChan <- struct{}{}
		closeConn()
	}()
	// プログラムを終了させようとしたときにsignalChanにシグナルを送る
	// 割り込みを表すSIGINTまたは停止を表すSIGTERMというUnixシグナルが使われる
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	if err := dialdb(); err != nil {
		log.Fatalln("MongoDBへのダイヤルに失敗しました:", err)
	}
	defer closedb()

	votes := make(chan string) // 投票結果のためのチャネル
	publisherStoppedChan := publishVotes(votes)
	twitterStoppedChan := startTwitterStream(stopChan, votes)
	// readFromTwitterは、調査の選択肢を毎回データベースから読み直す必要がある
	// プログラムの状態を常に最新に保つことも望まれる
	// 1分ごとにcloseConnを呼び出して接続を切断するgoroutine
	// 切断されるとreadFromTwitterが再び呼び出される
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			closeConn()
			// 2つのgoroutineがstop変数にアクセスしており、両者の競合を避けるためのLock
			stoplock.Lock()
			if stop {
				stoplock.Unlock()
				break
			}
			stoplock.Unlock()
		}
	}()
	<-twitterStoppedChan
	close(votes)
	<-publisherStoppedChan
}

var db *mongo.Client

func dialdb() error {
	var err error
	log.Println("MongoDBにダイヤル中")
	credential := options.Credential{
		Username: "root",
		Password: "password",
	}
	clientOpts := options.Client().ApplyURI("mongodb://mongo:27017").SetAuth(credential)
	db, err = mongo.Connect(context.TODO(), clientOpts)
	return err
}

func closedb() {
	if err := db.Disconnect(context.TODO()); err != nil {
		log.Fatalln("データベース接続を閉じることに失敗しました:", err)
	}
	log.Println("データベース接続が閉じられました")
}

type poll struct {
	Options []string
}

func loadOptions() ([]string, error) {
	var options []string
	// イテレータを取得
	filter := bson.D{}
	cursor, err := db.Database("ballots").Collection("polls").Find(context.TODO(), filter)
	if err != nil {
		log.Fatalln(err)
	}

	var results []poll
	if err = cursor.All(context.TODO(), &results); err != nil {
		log.Fatalln(err)
	}
	for _, result := range results {
		options = append(options, result.Options...)
	}

	return options, err
}

func publishVotes(votes <-chan string) <-chan struct{} {
	stopchan := make(chan struct{}, 1)
	// NSQのプロデューサーを生成し、localhost上のデフォルトのポートに接続
	pub, _ := nsq.NewProducer("nsqd:4150", nsq.NewConfig())
	go func() {
		// votesチャネルから定期的に値を読み出す
		for vote := range votes {
			pub.Publish("votes", []byte(vote)) // 投票内容をパブリッシュする
		}
		log.Println("Publisher: 停止中です")
		pub.Stop()
		log.Println("Publisher: 停止しました")
		stopchan <- struct{}{}
	}()
	return stopchan
}
