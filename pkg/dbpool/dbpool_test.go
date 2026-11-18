package dbpool

import (
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func TestResolveDefaults(t *testing.T) {
	cases := map[string]int{"canvas": 30, "studio": 30, "admin-api": 10, "gateway-api": 48, "note": 10, "unknown": 10}
	for svc, want := range cases {
		l := Resolve(svc, Config{})
		if l.MaxOpen != want || l.MaxIdle != min(want, 10) || l.Lifetime != 30*time.Minute {
			t.Errorf("%s: %+v", svc, l)
		}
	}
}

func TestResolveOverride(t *testing.T) {
	l := Resolve("canvas", Config{MaxOpenConns: 5, MaxIdleConns: 50, ConnMaxLifetimeSeconds: 60})
	if l.MaxOpen != 5 || l.MaxIdle != 5 || l.Lifetime != time.Minute {
		t.Fatalf("%+v", l)
	}
	l = Resolve("canvas", Config{MaxOpenConns: -1, MaxIdleConns: -1})
	if l.MaxOpen != 30 || l.MaxIdle != 10 {
		t.Fatalf("%+v", l)
	}
}

func TestApplyNilConn(t *testing.T) {
	if _, err := Apply("gateway-api", nil, Config{}); err != nil {
		t.Fatal(err)
	}
}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, driver.ErrBadConn }

func init() { sql.Register("dbpool-fake", fakeDriver{}) }

func TestApplyOnSqlConn(t *testing.T) {
	db, _ := sql.Open("dbpool-fake", "")
	conn := sqlx.NewSqlConnFromDB(db)
	l, err := Apply("studio", conn, Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := conn.RawDB()
	if got := raw.Stats().MaxOpenConnections; got != 30 || l.MaxOpen != 30 {
		t.Fatalf("MaxOpenConnections=%d", got)
	}
}

func TestMustApplyUnreachableDoesNotPanic(t *testing.T) {
	old := retryInterval
	retryInterval = time.Hour
	defer func() { retryInterval = old }()
	conn := sqlx.NewMysql("u:p@tcp(127.0.0.1:1)/x")
	if l := MustApply("note", conn, Config{}); l.MaxOpen != 10 {
		t.Fatalf("%+v", l)
	}
}

func TestDefaultsTotal(t *testing.T) {
	sum := 0
	for _, v := range ServiceDefaults {
		sum += v
	}
	if sum != 168 {
		t.Fatalf("total=%d", sum)
	}
}

func TestConfigYAMLOptional(t *testing.T) {
	var c struct {
		DataSource string
		DB         Config `json:",optional"`
	}
	if err := conf.LoadFromYamlBytes([]byte("DataSource: x\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.DB != (Config{}) {
		t.Fatalf("%+v", c.DB)
	}
	if err := conf.LoadFromYamlBytes([]byte("DataSource: x\nDB:\n  MaxOpenConns: 12\n  ConnMaxLifetimeSeconds: 90\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.DB.MaxOpenConns != 12 || c.DB.ConnMaxLifetimeSeconds != 90 {
		t.Fatalf("%+v", c.DB)
	}
}
