package firehosePool

import (
	"log"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/firehose"
)

const (
	connectionRetry = 2 * time.Second
	connectTimeout  = 15 * time.Second
	errorsFrame     = 10 * time.Second
	maxErrors       = 10 // Limit of errors to restart the connection

)

func (srv *Server) _reload() {
	for _ = range srv.reload {
		if srv.isExiting() {
			continue
		}

		if srv.C == nil {
			srv.C = make(chan []byte, srv.cfg.Buffer)
		}

		srv.Lock()
		srv.failing = true
		srv.Unlock()

		if err := srv.clientsReset(); err != nil {
			log.Printf("Firehose ERROR: can't connect to kinesis: %s", err)
			time.Sleep(connectionRetry)
		}
	}
}

func (srv *Server) failure() {
	if srv.isExiting() {
		return
	}

	srv.Lock()
	defer srv.Unlock()

	if time.Now().Sub(srv.lastError) > errorsFrame {
		srv.errors = 0
	}

	srv.errors++
	srv.lastError = time.Now()
	log.Printf("Firehose: %d errors detected", srv.errors)

	if srv.errors > maxErrors {
		srv.reConnect()
	}
}

func (srv *Server) reConnect() {
	select {
	case srv.reload <- true:
	default:
	}
}

func (srv *Server) clientsReset() (err error) {
	srv.Lock()
	defer srv.Unlock()

	srv.reseting = true
	defer func() { srv.reseting = false }()

	log.Printf("Firehose Reload config to the stream %s", srv.cfg.StreamName)

	var sess *session.Session

	if srv.cfg.Profile != "" {
		sess, err = session.NewSessionWithOptions(session.Options{Profile: srv.cfg.Profile})
	} else {
		sess, err = session.NewSession()
	}

	if err != nil {
		log.Printf("Firehose ERROR: session: %s", err)

		srv.errors++
		srv.lastError = time.Now()
		return err
	}

	srv.awsSvc = firehose.New(sess, &aws.Config{Region: aws.String(srv.cfg.Region)})
	stream := &firehose.DescribeDeliveryStreamInput{
		DeliveryStreamName: &srv.cfg.StreamName,
	}

	var l *firehose.DescribeDeliveryStreamOutput
	l, err = srv.awsSvc.DescribeDeliveryStream(stream)
	if err != nil {
		log.Printf("Firehose ERROR: describe stream: %s", err)

		srv.errors++
		srv.lastError = time.Now()
		return err
	}

	log.Printf("Firehose Connected to %s (%s) status %s",
		*l.DeliveryStreamDescription.DeliveryStreamName,
		*l.DeliveryStreamDescription.DeliveryStreamARN,
		*l.DeliveryStreamDescription.DeliveryStreamStatus)

	srv.lastConnection = time.Now()
	srv.errors = 0
	srv.failing = false

	currClients := len(srv.clients)

	// No changes in the number of clients
	if currClients == srv.cfg.Workers {
		return nil
	}

	// If the config define lower number than the active clients remove the difference
	if currClients > srv.cfg.Workers {
		for i := currClients; i > srv.cfg.Workers; i-- {
			k := i - 1
			srv.clients[k].Exit()
			srv.clients = append(srv.clients[:k], srv.clients[k+1:]...)
		}
	} else {
		// If the config define higher number than the active clients start new clients
		for i := currClients; i < srv.cfg.Workers; i++ {
			srv.clients = append(srv.clients, NewClient(srv))
		}
	}

	return nil
}