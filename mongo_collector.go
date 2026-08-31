package main

// ============================================================================
// mongodb 类型采集器 — general-exporter 扩展
//
// 本文件与 main.go 同 package，放到仓库根目录即可参与编译。
// 对 main.go 的改动点见同目录 README.md「集成步骤」（共 6 处）。
//
// 设计语义（与用户确认）：
//   - metrics 只做"简单记录"：从查询结果文档取字段，数值原样输出，
//     不做单位换算、不派生 ok 指标，判断全部交给 PromQL。
//   - 方案 A 兜底：每 target 自动附带固定指标 mongodb_probe_up
//     （查询成功=1，失败=0），靠 name 标签区分 target。
//   - A1 行为：查询失败时只发 up=0，数值指标不发（防止用旧值误判）。
//   - query 省略 = 取集合最新一条（find sort {_id:-1} limit 1）；
//     query 有值 = FindOne 精确过滤（兼容 {processName} 型进度表）。
// ============================================================================

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ==================== Mongo 配置 ====================

// MongoMetric 定义一个输出指标：从文档 field 取值，输出为 gauge name。
type MongoMetric struct {
	Name  string `yaml:"name"`
	Field string `yaml:"field"`
}

// 需要在 main.go 的 TargetConfig 中追加以下平铺字段（与 url 类型风格一致）：
//
//	URI        string         `yaml:"uri"`        // MongoDB 连接串
//	Database   string         `yaml:"database"`   // 库名
//	Collection string         `yaml:"collection"` // 集合名（进度表）
//	Query      map[string]any `yaml:"query"`      // 可选过滤条件；空 = 取最新一条
//	Metrics    []MongoMetric  `yaml:"metrics"`    // 取值字段列表

// ==================== Mongo Metric Registry ====================

// MongoMetricRegistry 按指标名共享 GaugeVec（仿 CustomMetricRegistry），
// 多个 target 相同指标名时靠 name/env/tier/global 标签区分。
// 与 Custom 不同：指标名是声明式的，不需要 PurgeStale。
type MongoMetricRegistry struct {
	mu           sync.Mutex
	gauges       map[string]*prometheus.GaugeVec
	reg          *prometheus.Registry
	globalKeys   []string
	globalValues map[string]string
}

func NewMongoMetricRegistry(reg *prometheus.Registry, globalKeys []string, globalValues map[string]string) *MongoMetricRegistry {
	return &MongoMetricRegistry{
		gauges:       make(map[string]*prometheus.GaugeVec),
		reg:          reg,
		globalKeys:   globalKeys,
		globalValues: globalValues,
	}
}

func (r *MongoMetricRegistry) GetOrCreateGauge(name string) *prometheus.GaugeVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	labels := append([]string{"name", "env", "tier"}, r.globalKeys...)
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: name,
		Help: "MongoDB metric from configured query",
	}, labels)
	r.reg.MustRegister(g)
	r.gauges[name] = g
	return g
}

// ResetAll 每轮采集前清空所有值；未被 Set 的 label 组合自然不输出。
func (r *MongoMetricRegistry) ResetAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.gauges {
		g.Reset()
	}
}

// ==================== Mongo Collector ====================

// 固定兜底指标名：方案 A（查询成功=1，失败=0），靠 name 标签区分 target。
const mongodbUpMetric = "mongodb_probe_up"

type MongoCollector struct {
	cfg          TargetConfig
	timeout      time.Duration
	reg          *MongoMetricRegistry
	globalValues map[string]string

	clientOnce sync.Once
	client     *mongo.Client
	connectErr error
}

func NewMongoCollector(cfg TargetConfig, defaults DefaultConfig, reg *MongoMetricRegistry, globalValues map[string]string) *MongoCollector {
	return &MongoCollector{
		cfg:          cfg,
		timeout:      parseDuration(defaults.Timeout, "30s"),
		reg:          reg,
		globalValues: globalValues,
	}
}

// getClient 懒连接 + Ping 校验；连接失败会在后续 Collect 中反复重试。
func (c *MongoCollector) getClient(ctx context.Context) (*mongo.Client, error) {
	c.clientOnce.Do(func() {
		c.client, c.connectErr = mongo.Connect(ctx, options.Client().ApplyURI(c.cfg.URI))
		if c.connectErr == nil {
			c.connectErr = c.client.Ping(ctx, nil)
		}
	})
	return c.client, c.connectErr
}

// Close 释放连接池；SIGHUP 热加载重建时对旧 collectors 调用。
func (c *MongoCollector) Close() {
	if c.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = c.client.Disconnect(ctx)
	}
}

func (c *MongoCollector) Collect() {
	lv := prometheus.Labels{
		"name": c.cfg.Name,
		"env":  c.cfg.Labels["env"],
		"tier": c.cfg.Labels["tier"],
	}
	for k, v := range c.globalValues {
		lv[k] = v
	}
	up := c.reg.GetOrCreateGauge(mongodbUpMetric)

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	cli, err := c.getClient(ctx)
	if err != nil {
		log.Printf("[mongo:%s] connect failed: %v", c.cfg.Name, err)
		up.With(lv).Set(0) // A1: 失败只发 up=0
		return
	}

	doc, err := c.findOne(ctx, cli)
	if err != nil {
		log.Printf("[mongo:%s] query failed: %v", c.cfg.Name, err)
		up.With(lv).Set(0)
		return
	}
	if doc == nil {
		log.Printf("[mongo:%s] no document matched", c.cfg.Name)
		up.With(lv).Set(0)
		return
	}

	// metrics 简单记录：字段原样输出，不做任何加工
	for _, m := range c.cfg.Metrics {
		v, ok := doc[m.Field]
		if !ok {
			log.Printf("[mongo:%s] field %q not found in document", c.cfg.Name, m.Field)
			continue
		}
		f, ok := toFloat64(v)
		if !ok {
			log.Printf("[mongo:%s] field %q has non-numeric value %T", c.cfg.Name, m.Field, v)
			continue
		}
		c.reg.GetOrCreateGauge(m.Name).With(lv).Set(f)
	}
	up.With(lv).Set(1)
}

// findOne 按配置取文档：有 query 则精确过滤，无 query 取最新一条。
func (c *MongoCollector) findOne(ctx context.Context, cli *mongo.Client) (bson.M, error) {
	db := cli.Database(c.cfg.Database)
	coll := db.Collection(c.cfg.Collection)

	if len(c.cfg.Query) > 0 {
		var doc bson.M
		err := coll.FindOne(ctx, bson.M(c.cfg.Query)).Decode(&doc)
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return doc, err
	}

	cur, err := coll.Find(ctx, bson.M{}, options.Find().SetSort(bson.M{"_id": -1}).SetLimit(1))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, nil
	}
	return docs[0], nil
}

// toFloat64 把 bson 文档字段值转 float64；数字原样，字符串数字解析，
// time.Time 转 Unix 毫秒（与 updateAt=Date.now() 的毫秒语义一致），其余不支持。
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	case time.Time:
		return float64(n.UnixMilli()), true
	default:
		return 0, false
	}
}
