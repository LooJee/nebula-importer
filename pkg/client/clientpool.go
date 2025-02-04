package client

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	nebula "github.com/vesoft-inc/nebula-go/v3"
	"github.com/vesoft-inc/nebula-importer/v3/pkg/base"
	"github.com/vesoft-inc/nebula-importer/v3/pkg/config"
	"github.com/vesoft-inc/nebula-importer/v3/pkg/logger"
)

const (
	DefaultRetryInitialInterval     = time.Second
	DefaultRetryRandomizationFactor = 0.1
	DefaultRetryMultiplier          = 1.5
	DefaultRetryMaxInterval         = 2 * time.Minute
	DefaultRetryMaxElapsedTime      = time.Hour
)

type ClientPool struct {
	retry        int
	concurrency  int
	space        string
	postStart    *config.NebulaPostStart
	preStop      *config.NebulaPreStop
	statsCh      chan<- base.Stats
	pool         *nebula.ConnectionPool
	Sessions     []*nebula.Session
	requestChs   []chan base.ClientRequest
	runnerLogger *logger.RunnerLogger
}

func NewClientPool(settings *config.NebulaClientSettings, statsCh chan<- base.Stats, runnerLogger *logger.RunnerLogger) (*ClientPool, error) {
	addrs := strings.Split(*settings.Connection.Address, ",")
	var hosts []nebula.HostAddress
	for _, addr := range addrs {
		hostPort := strings.Split(addr, ":")
		if len(hostPort) != 2 {
			return nil, fmt.Errorf("Invalid address: %s", addr)
		}
		port, err := strconv.Atoi(hostPort[1])
		if err != nil {
			return nil, err
		}
		hostAddr := nebula.HostAddress{Host: hostPort[0], Port: port}
		hosts = append(hosts, hostAddr)
	}
	conf := nebula.PoolConfig{
		TimeOut:         0,
		IdleTime:        0,
		MaxConnPoolSize: len(addrs) * *settings.Concurrency,
		MinConnPoolSize: 1,
	}
	connPool, err := nebula.NewConnectionPool(hosts, conf, logger.NewNebulaLogger(runnerLogger))
	if err != nil {
		return nil, err
	}
	pool := ClientPool{
		space:        *settings.Space,
		postStart:    settings.PostStart,
		preStop:      settings.PreStop,
		statsCh:      statsCh,
		pool:         connPool,
		runnerLogger: runnerLogger,
	}
	pool.retry = *settings.Retry
	pool.concurrency = (*settings.Concurrency) * len(addrs)
	pool.Sessions = make([]*nebula.Session, pool.concurrency)
	pool.requestChs = make([]chan base.ClientRequest, pool.concurrency)

	j := 0
	for k := 0; k < len(addrs); k++ {
		for i := 0; i < *settings.Concurrency; i++ {
			if pool.Sessions[j], err = pool.pool.GetSession(*settings.Connection.User, *settings.Connection.Password); err != nil {
				return nil, err
			}
			pool.requestChs[j] = make(chan base.ClientRequest, *settings.ChannelBufferSize)
			j++
		}
	}

	return &pool, nil
}

func (p *ClientPool) getActiveConnIdx() int {
	for i := range p.Sessions {
		if p.Sessions[i] != nil {
			return i
		}
	}
	return -1
}

func (p *ClientPool) exec(i int, stmt string) error {
	if len(stmt) == 0 {
		return nil
	}
	resp, err := p.Sessions[i].Execute(stmt)
	if err != nil {
		return fmt.Errorf("Client(%d) fails to execute commands (%s), error: %s", i, stmt, err.Error())
	}

	if !resp.IsSucceed() {
		return fmt.Errorf("Client(%d) fails to execute commands (%s), response error code: %v, message: %s",
			i, stmt, resp.GetErrorCode(), resp.GetErrorMsg())
	}

	return nil
}

func (p *ClientPool) Close() {
	if p.preStop != nil && p.preStop.Commands != nil {
		if i := p.getActiveConnIdx(); i != -1 {
			if err := p.exec(i, *p.preStop.Commands); err != nil {
				logger.Log.Errorf("%s", err.Error())
			}
		}
	}

	for i := 0; i < p.concurrency; i++ {
		if p.Sessions[i] != nil {
			p.Sessions[i].Release()
		}
		if p.requestChs[i] != nil {
			close(p.requestChs[i])
		}
	}
	p.pool.Close()
}

func (p *ClientPool) Init() error {
	i := p.getActiveConnIdx()
	if i == -1 {
		return fmt.Errorf("no available session.")
	}
	if p.postStart != nil && p.postStart.Commands != nil {
		if err := p.exec(i, *p.postStart.Commands); err != nil {
			return err
		}
	}

	if p.postStart != nil {
		afterPeriod, _ := time.ParseDuration(*p.postStart.AfterPeriod)
		time.Sleep(afterPeriod)
	}

	// pre-check for use space statement
	if err := p.exec(i, fmt.Sprintf("USE `%s`;", p.space)); err != nil {
		return err
	}

	for i := 0; i < p.concurrency; i++ {
		go func(i int) {
			p.startWorker(i)
		}(i)
	}
	return nil
}

func (p *ClientPool) startWorker(i int) {
	stmt := fmt.Sprintf("USE `%s`;", p.space)
	if err := p.exec(i, stmt); err != nil {
		logger.Log.Error(err.Error())
		return
	}
	for {
		data, ok := <-p.requestChs[i]
		if !ok {
			break
		}

		if data.Stmt == base.STAT_FILEDONE {
			data.ErrCh <- base.ErrData{Error: nil}
			continue
		}

		now := time.Now()

		exp := backoff.NewExponentialBackOff()
		exp.InitialInterval = DefaultRetryInitialInterval
		exp.RandomizationFactor = DefaultRetryRandomizationFactor
		exp.Multiplier = DefaultRetryMultiplier
		exp.MaxInterval = DefaultRetryMaxInterval
		exp.MaxElapsedTime = DefaultRetryMaxElapsedTime

		var (
			err   error
			resp  *nebula.ResultSet
			retry = p.retry
		)

		// There are three cases of retry
		// * Case 1: retry no more
		// * Case 2. retry as much as possible
		// * Case 3: retry with limit times
		_ = backoff.Retry(func() error {
			resp, err = p.Sessions[i].Execute(data.Stmt)
			if err == nil && resp.IsSucceed() {
				return nil
			}
			retryErr := err
			if resp != nil {
				errorCode, errorMsg := resp.GetErrorCode(), resp.GetErrorMsg()
				retryErr = fmt.Errorf("%d:%s", errorCode, errorMsg)

				// Case 1: retry no more
				var isPermanentError = true
				switch errorCode {
				case nebula.ErrorCode_E_SYNTAX_ERROR:
				case nebula.ErrorCode_E_SEMANTIC_ERROR:
				default:
					isPermanentError = false
				}
				if isPermanentError {
					// stop the retry
					return backoff.Permanent(retryErr)
				}

				// Case 2. retry as much as possible
				// TODO: compare with E_RAFT_BUFFER_OVERFLOW
				// Can not get the E_RAFT_BUFFER_OVERFLOW inside storage now.
				if strings.Contains(errorMsg, "raft buffer is full") {
					retry = p.retry
					return retryErr
				}
			}
			// Case 3: retry with limit times
			if retry <= 0 {
				// stop the retry
				return backoff.Permanent(retryErr)
			}
			retry--
			return retryErr
		}, exp)

		if err != nil {
			err = fmt.Errorf("Client %d fail to execute: %s, Error: %s", i, data.Stmt, err.Error())
		} else {
			if !resp.IsSucceed() {
				err = fmt.Errorf("Client %d fail to execute: %s, ErrMsg: %s, ErrCode: %v", i, data.Stmt, resp.GetErrorMsg(), resp.GetErrorCode())
			}
		}

		if err != nil {
			data.ErrCh <- base.ErrData{
				Error: err,
				Data:  data.Data,
			}
		} else {
			timeInMs := time.Since(now).Nanoseconds() / 1e3
			var importedBytes int64
			for _, d := range data.Data {
				importedBytes += int64(d.Bytes)
			}
			p.statsCh <- base.NewSuccessStats(int64(resp.GetLatency()), timeInMs, len(data.Data), importedBytes)
		}
	}
}
