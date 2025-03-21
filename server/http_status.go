// Copyright 2025 Ekjot Singh
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"runtime"
	rpprof "runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/printer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tiancaiamao/appdash/traceapp"
	"go.uber.org/zap"
	static "sourcegraph.com/sourcegraph/appdash-data"
)

const defaultStatusPort = 10080

func (s *Server) startStatusHTTP() {
	go s.startHTTPServer()
}

func serveError(w http.ResponseWriter, status int, txt string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Go-Pprof", "1")
	w.Header().Del("Content-Disposition")
	w.WriteHeader(status)
	_, err := fmt.Fprintln(w, txt)
	terror.Log(err)
}

func sleepWithCtx(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func (s *Server) startHTTPServer() {
	router := mux.NewRouter()

	router.HandleFunc("/status", s.handleStatus).Name("Status")
	// HTTP path for prometheus.
	router.Handle("/metrics", prometheus.Handler()).Name("Metrics")

	// HTTP path for dump statistics.
	router.Handle("/stats/dump/{db}/{table}", s.newStatsHandler()).Name("StatsDump")
	router.Handle("/stats/dump/{db}/{table}/{snapshot}", s.newStatsHistoryHandler()).Name("StatsHistoryDump")

	router.Handle("/settings", settingsHandler{}).Name("Settings")
	router.Handle("/reload-config", configReloadHandler{}).Name("ConfigReload")
	router.Handle("/binlog/recover", binlogRecover{}).Name("BinlogRecover")

	tikvHandlerTool := s.newTikvHandlerTool()
	router.Handle("/schema", schemaHandler{tikvHandlerTool}).Name("Schema")
	router.Handle("/schema/{db}", schemaHandler{tikvHandlerTool})
	router.Handle("/schema/{db}/{table}", schemaHandler{tikvHandlerTool})
	router.Handle("/tables/{colID}/{colTp}/{colFlag}/{colLen}", valueHandler{})
	router.Handle("/ddl/history", ddlHistoryJobHandler{tikvHandlerTool}).Name("DDL_History")
	router.Handle("/ddl/owner/resign", ddlResignOwnerHandler{tikvHandlerTool.Store.(kv.Storage)}).Name("DDL_Owner_Resign")

	// HTTP path for get server info.
	router.Handle("/info", serverInfoHandler{tikvHandlerTool}).Name("Info")
	router.Handle("/info/all", allServerInfoHandler{tikvHandlerTool}).Name("InfoALL")
	// HTTP path for get db and table info that is related to the tableID.
	router.Handle("/db-table/{tableID}", dbTableHandler{tikvHandlerTool})

	if s.cfg.Store == "tikv" {
		// HTTP path for tikv.
		router.Handle("/tables/{db}/{table}/regions", tableHandler{tikvHandlerTool, opTableRegions})
		router.Handle("/tables/{db}/{table}/scatter", tableHandler{tikvHandlerTool, opTableScatter})
		router.Handle("/tables/{db}/{table}/stop-scatter", tableHandler{tikvHandlerTool, opStopTableScatter})
		router.Handle("/tables/{db}/{table}/disk-usage", tableHandler{tikvHandlerTool, opTableDiskUsage})
		router.Handle("/regions/meta", regionHandler{tikvHandlerTool}).Name("RegionsMeta")
		router.Handle("/regions/hot", regionHandler{tikvHandlerTool}).Name("RegionHot")
		router.Handle("/regions/{regionID}", regionHandler{tikvHandlerTool})
		router.Handle("/mvcc/key/{db}/{table}/{handle}", mvccTxnHandler{tikvHandlerTool, opMvccGetByKey})
		router.Handle("/mvcc/txn/{startTS}/{db}/{table}", mvccTxnHandler{tikvHandlerTool, opMvccGetByTxn})
		router.Handle("/mvcc/hex/{hexKey}", mvccTxnHandler{tikvHandlerTool, opMvccGetByHex})
		router.Handle("/mvcc/index/{db}/{table}/{index}/{handle}", mvccTxnHandler{tikvHandlerTool, opMvccGetByIdx})
	}
	addr := fmt.Sprintf("%s:%d", s.cfg.Status.StatusHost, s.cfg.Status.StatusPort)
	if s.cfg.Status.StatusPort == 0 {
		addr = fmt.Sprintf("%s:%d", s.cfg.Status.StatusHost, defaultStatusPort)
	}

	// HTTP path for web UI.
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if host == "" {
			host = "localhost"
		}
		baseURL := &url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%s", host, port),
		}
		router.HandleFunc("/web/trace", traceapp.HandleTiDB).Name("Trace Viewer")
		sr := router.PathPrefix("/web/trace/").Subrouter()
		if _, err := traceapp.New(traceapp.NewRouter(sr), baseURL); err != nil {
			logutil.Logger(context.Background()).Error("new failed", zap.Error(err))
		}
		router.PathPrefix("/static/").Handler(http.StripPrefix("/static", http.FileServer(static.Data)))
	}

	serverMux := http.NewServeMux()
	serverMux.Handle("/", router)

	serverMux.HandleFunc("/debug/pprof/", pprof.Index)
	serverMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	serverMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	serverMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	serverMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	serverMux.HandleFunc("/debug/zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="tidb_debug"`+time.Now().Format("20060102150405")+".zip"))

		// dump goroutine/heap/mutex
		items := []struct {
			name   string
			gc     int
			debug  int
			second int
		}{
			{name: "goroutine", debug: 2},
			{name: "heap", gc: 1},
			{name: "mutex"},
		}
		zw := zip.NewWriter(w)
		for _, item := range items {
			p := rpprof.Lookup(item.name)
			if p == nil {
				serveError(w, http.StatusNotFound, "Unknown profile")
				return
			}
			if item.gc > 0 {
				runtime.GC()
			}
			fw, err := zw.Create(item.name)
			if err != nil {
				serveError(w, http.StatusInternalServerError, fmt.Sprintf("Create zipped %s fail: %v", item.name, err))
				return
			}
			err = p.WriteTo(fw, item.debug)
			terror.Log(err)
		}

		// dump profile
		fw, err := zw.Create("profile")
		if err != nil {
			serveError(w, http.StatusInternalServerError, fmt.Sprintf("Create zipped %s fail: %v", "profile", err))
			return
		}
		if err := rpprof.StartCPUProfile(fw); err != nil {
			serveError(w, http.StatusInternalServerError,
				fmt.Sprintf("Could not enable CPU profiling: %s", err))
			return
		}
		sec, err := strconv.ParseInt(r.FormValue("seconds"), 10, 64)
		if sec <= 0 || err != nil {
			sec = 10
		}
		sleepWithCtx(r.Context(), time.Duration(sec)*time.Second)
		rpprof.StopCPUProfile()

		// dump config
		fw, err = zw.Create("config")
		if err != nil {
			serveError(w, http.StatusInternalServerError, fmt.Sprintf("Create zipped %s fail: %v", "config", err))
			return
		}
		js, err := json.MarshalIndent(config.GetGlobalConfig(), "", " ")
		if err != nil {
			serveError(w, http.StatusInternalServerError, fmt.Sprintf("get config info fail%v", err))
			return
		}
		_, err = fw.Write(js)
		terror.Log(err)

		// dump version
		fw, err = zw.Create("version")
		if err != nil {
			serveError(w, http.StatusInternalServerError, fmt.Sprintf("Create zipped %s fail: %v", "version", err))
			return
		}
		_, err = fw.Write([]byte(printer.GetTiDBInfo()))
		terror.Log(err)

		err = zw.Close()
		terror.Log(err)
	})
	fetcher := sqlInfoFetcher{store: tikvHandlerTool.Store}
	serverMux.HandleFunc("/debug/sub-optimal-plan", fetcher.zipInfoForSQL)

	var (
		httpRouterPage bytes.Buffer
		pathTemplate   string
		err            error
	)
	httpRouterPage.WriteString("<html><head><title>TiDB Status and Metrics Report</title></head><body><h1>TiDB Status and Metrics Report</h1><table>")
	err = router.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		pathTemplate, err = route.GetPathTemplate()
		if err != nil {
			logutil.Logger(context.Background()).Error("get HTTP router path failed", zap.Error(err))
		}
		name := route.GetName()
		// If the name attribute is not set, GetName returns "".
		// "traceapp.xxx" are introduced by the traceapp package and are also ignored.
		if name != "" && !strings.HasPrefix(name, "traceapp") && err == nil {
			httpRouterPage.WriteString("<tr><td><a href='" + pathTemplate + "'>" + name + "</a><td></tr>")
		}
		return nil
	})
	if err != nil {
		logutil.Logger(context.Background()).Error("generate root failed", zap.Error(err))
	}
	httpRouterPage.WriteString("<tr><td><a href='/debug/pprof/'>Debug</a><td></tr>")
	httpRouterPage.WriteString("</table></body></html>")
	router.HandleFunc("/", func(responseWriter http.ResponseWriter, request *http.Request) {
		_, err = responseWriter.Write([]byte(httpRouterPage.String()))
		if err != nil {
			logutil.Logger(context.Background()).Error("write HTTP index page failed", zap.Error(err))
		}
	})

	logutil.Logger(context.Background()).Info("for status and metrics report", zap.String("listening on addr", addr))
	s.statusServer = &http.Server{Addr: addr, Handler: CorsHandler{handler: serverMux, cfg: s.cfg}}

	if len(s.cfg.Security.ClusterSSLCA) != 0 {
		err = s.statusServer.ListenAndServeTLS(s.cfg.Security.ClusterSSLCert, s.cfg.Security.ClusterSSLKey)
	} else {
		err = s.statusServer.ListenAndServe()
	}

	if err != nil {
		logutil.Logger(context.Background()).Info("listen failed", zap.Error(err))
	}
}

// status of TiDB.
type status struct {
	Connections int    `json:"connections"`
	Version     string `json:"version"`
	GitHash     string `json:"git_hash"`
}

func (s *Server) handleStatus(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	st := status{
		Connections: s.ConnectionCount(),
		Version:     mysql.ServerVersion,
		GitHash:     printer.TiDBGitHash,
	}
	js, err := json.Marshal(st)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		logutil.Logger(context.Background()).Error("encode json failed", zap.Error(err))
	} else {
		_, err = w.Write(js)
		terror.Log(errors.Trace(err))
	}
}
