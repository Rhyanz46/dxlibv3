package database

import (
	"context"
	"database/sql"
	"fmt"
	goOra "github.com/sijms/go-ora/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	pq "github.com/knetic/go-namedparameterquery"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"
	_ "github.com/sijms/go-ora/v2"

	"dxlib/configuration"
	"dxlib/database/database_type"
	"dxlib/database/protected/db"
	"dxlib/database/protected/dbtx"
	"dxlib/log"
	"dxlib/utils"
	utilsSql "dxlib/utils/security"
)

type DXDatabaseEventFunc func(dm *DXDatabase, err error)

type DXDatabaseTxCallback func(dtx *DXDatabaseTx) (err error)

type DXDatabaseTxIsolationLevel = sql.IsolationLevel

const (
	LevelDefault DXDatabaseTxIsolationLevel = iota
	LevelReadUncommitted
	LevelReadCommitted
	LevelWriteCommitted
	LevelRepeatableRead
	LevelSnapshot
	LevelSerializable
	LevelLinearizable
)

type DXDatabaseTx struct {
	*sqlx.Tx
	Log *log.DXLog
}

type DXDatabase struct {
	NameId                       string
	IsConfigured                 bool
	DatabaseType                 database_type.DXDatabaseType
	Address                      string
	UserName                     string
	UserPassword                 string
	DatabaseName                 string
	ConnectionOptions            string
	IsConnectAtStart             bool
	MustConnected                bool
	Connected                    bool
	Connection                   *sqlx.DB
	ConnectionString             string
	NonSensitiveConnectionString string
	OnCannotConnect              DXDatabaseEventFunc
	CreateScriptFiles            []string
}

func (d *DXDatabase) CheckConnection() (err error) {
	dbConn, err := d.Connection.Conn(context.Background())
	if err != nil {
		log.Log.Warnf("Database %v CheckConnection() failed: %v", d.NameId, err.Error())
		d.Connected = false
		return err
	}
	defer func() {
		_ = dbConn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	if err := dbConn.PingContext(ctx); err != nil {
		d.Connected = false
		log.Log.Warnf("Database %v ping failed: %v", d.NameId, err.Error())
		return err
	}
	log.Log.Tracef("Database %v ping success with result CheckConnection: %v", d.NameId, d.Connected)
	d.Connected = true
	return err
}

func (d *DXDatabase) CheckConnectionAndReconnect() (err error) {
	tryReconnect := false
	if d.Connected {
		err = d.CheckConnection()
		if err != nil {
			tryReconnect = true
		}
		if !d.Connected {
			tryReconnect = true
		}
	} else {
		tryReconnect = true
	}
	if tryReconnect {
		time.Sleep(1 * time.Second)
		err = d.Connect()
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *DXDatabase) ExecuteScript(s *DXDatabaseScript) (err error) {
	_, err = s.Execute(d)
	if err != nil {
		return err
	}
	return nil
}

func (d *DXDatabase) GetNonSensitiveConnectionString() string {
	return fmt.Sprintf("%s://%s/%s", d.DatabaseType.String(), d.Address, d.DatabaseName)
}

func (d *DXDatabase) GetConnectionString() (s string, err error) {
	switch d.DatabaseType {
	case database_type.PostgreSQL:
		s = fmt.Sprintf("%s://%s:%s@%s/%s?%s", d.DatabaseType.String(), d.UserName, d.UserPassword, d.Address, d.DatabaseName, d.ConnectionOptions)
	case database_type.SQLServer:
		host, port, err := net.SplitHostPort(d.Address)
		if err != nil {
			return "", err
		}
		s = fmt.Sprintf("server=%s;port=%s;user id=%s;password=%s;database=%s;encrypt=disable", host, port, d.UserName, d.UserPassword, d.DatabaseName)
	case database_type.Oracle:
		host, port, err := net.SplitHostPort(d.Address)
		if err != nil {
			return "", err
		}
		portInt, err := strconv.Atoi(port)
		if err != nil {
			return "", err
		}
		urlOptions := map[string]string{
			"SID": d.DatabaseName,
		}
		s = goOra.BuildUrl(host, portInt, "", d.UserName, d.UserPassword, urlOptions)
		/*s = fmt.Sprintf("%s/%s@%s/%s", d.UserName, d.UserPassword, d.Address, d.DatabaseName)
		/*host, port, err := net.SplitHostPort(d.Address)
		if err != nil {
			return "", err
		}
		s = fmt.Sprintf(`user="%s" password="%s" connectString="(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=%s)(PORT=%s))(CONNECT_DATA=(SERVICE_NAME=%s)))"`,
			d.UserName,
			d.UserPassword,
			host,
			port,
			d.DatabaseName,
		)*/
	default:
		err = log.Log.ErrorAndCreateErrorf("configuration is unusable, value of database_type field of database %s configuration is not supported (%s)", d.NameId, s)
	}
	return s, err
}

func (d *DXDatabase) ApplyFromConfiguration() (err error) {
	if !d.IsConfigured {
		log.Log.Infof("Configuring to Database %s... start", d.NameId)
		configurationData, ok := configuration.Manager.Configurations["storage"]
		if !ok {
			err = log.Log.PanicAndCreateErrorf("DXDatabase/ApplyFromConfiguration/1", "Storage configuration not found")
			return err
		}
		m := *(configurationData.Data)
		databaseConfiguration, ok := m[d.NameId].(utils.JSON)
		if !ok {
			if d.MustConnected {
				err := log.Log.FatalAndCreateErrorf("Database %s configuration not found", d.NameId)
				return err
			} else {
				err := log.Log.WarnAndCreateErrorf("Manager is unusable, database %s configuration not found", d.NameId)
				return err
			}
		}
		n, ok := databaseConfiguration[`nameid`].(string)
		if ok {
			d.NameId = n
		}
		b, ok := databaseConfiguration[`must_connected`].(bool)
		if ok {
			d.MustConnected = b
		}
		b, ok = databaseConfiguration[`is_connect_at_start`].(bool)
		if ok {
			d.IsConnectAtStart = b
		}
		s, ok := databaseConfiguration[`database_type`].(string)
		if !ok {
			if d.MustConnected {
				err := log.Log.FatalAndCreateErrorf("Mandatory database_type field value in database %s configuration is not supported (%v)", d.NameId, s)
				return err
			} else {
				err := log.Log.WarnAndCreateErrorf("configuration is unusable, mandatory database_type field value database %s configuration  is not supported (%v)", d.NameId, s)
				return err
			}
		}
		d.DatabaseType = database_type.StringToDXDatabaseType(s)
		if d.DatabaseType == database_type.UnknownDatabaseType {
			if d.MustConnected {
				err := log.Log.FatalAndCreateErrorf("Mandatory value of database_type field of Database %s configuration is not supported (%s)", d.NameId, s)
				return err
			} else {
				err := log.Log.WarnAndCreateErrorf("configuration is unusable, value of database_type field of database %s configuration is not supported (%s)", d.NameId, s)
				return err
			}
		}
		d.Address, ok = databaseConfiguration[`address`].(string)
		if !ok {
			if d.MustConnected {
				err := log.Log.FatalAndCreateErrorf("Mandatory address field in Database %s configuration not exist", d.NameId)
				return err
			} else {
				err := log.Log.WarnAndCreateErrorf("configuration is unusable, mandatory address field in database %s configuration not exist", d.NameId)
				return err
			}
		}
		d.UserName, ok = databaseConfiguration[`user_name`].(string)
		if !ok {
			if d.MustConnected {
				err := log.Log.FatalAndCreateErrorf("Mandatory user_name field in Database %s configuration not exist", d.NameId)
				return err
			} else {
				err := log.Log.WarnAndCreateErrorf("configuration is unusable, mandatory user_name field in Database %s configuration not exist", d.NameId)
				return err
			}
		}
		d.UserPassword, ok = databaseConfiguration[`user_password`].(string)
		if !ok {
			if d.MustConnected {
				err := log.Log.FatalAndCreateErrorf("Mandatory user_password field in Database %s configuration not exist", d.NameId)
				return err
			} else {
				err := log.Log.WarnAndCreateErrorf("configuration is unusable, mandatory user_password field in Database %s configuration not exist", d.NameId)
				return err
			}
		}
		d.DatabaseName, ok = databaseConfiguration[`database_name`].(string)
		if !ok {
			if d.MustConnected {
				err := log.Log.FatalAndCreateErrorf("Mandatory database_name field in Database %s configuration not exist", d.NameId)
				return err
			} else {
				err := log.Log.WarnAndCreateErrorf("configuration is unusable, mandatory database_name field in Database %s configuration not exist", d.NameId)
				return err
			}
		}
		d.CreateScriptFiles, _ = databaseConfiguration[`create_script_files`].([]string)
		d.ConnectionOptions, _ = databaseConfiguration[`connection_options`].(string)

		d.NonSensitiveConnectionString = d.GetNonSensitiveConnectionString()
		d.ConnectionString, err = d.GetConnectionString()
		if err != nil {
			return err
		}
		log.Log.Infof("Connecting to Database %s... done", d.NonSensitiveConnectionString)
		d.IsConfigured = true
		log.Log.Infof("Configuring to Database %s... done", d.NameId)
	}
	return nil
}

func (d *DXDatabase) CheckIsErrorBecauseDbNotExist(err error) bool {
	s := err.Error()
	switch d.DatabaseType {
	case database_type.PostgreSQL:
		t1 := strings.Contains(s, "database")
		t2 := strings.Contains(s, "not exist")
		t3 := strings.Contains(s, d.DatabaseName)
		if t1 && t2 && t3 {
			return true
		}
	default:
		return false
	}
	return false
}

func (d *DXDatabase) Connect() (err error) {
	if !d.Connected {
		log.Log.Infof("Connecting to database %s/%s... start", d.NameId, d.NonSensitiveConnectionString)
		connection, err := sqlx.Open(d.DatabaseType.Driver(), d.ConnectionString)
		if err != nil {
			if d.MustConnected {
				log.Log.Fatalf("Invalid parameters to open database %s/%s (%s)", d.NameId, d.NonSensitiveConnectionString, err.Error())
				return nil
			} else {
				log.Log.Errorf("Invalid parameters to open database %s/%s (%s)", d.NameId, d.NonSensitiveConnectionString, err.Error())
				return err
			}
		}
		d.Connection = connection
		err = connection.Ping()
		if err != nil {
			if d.OnCannotConnect != nil {
				d.OnCannotConnect(d, err)
			}
			if d.MustConnected {
				log.Log.Fatalf("Cannot connect and ping to database %s/%s (%s)", d.NameId, d.NonSensitiveConnectionString, err.Error())
				return nil
			} else {
				log.Log.Errorf("Cannot connect and ping to database %s/%s (%s)", d.NameId, d.NonSensitiveConnectionString, err.Error())
				return err
			}
		}
		d.Connected = true
		log.Log.Infof("Connecting to database %s/%s... done CONNECTED", d.NameId, d.NonSensitiveConnectionString)
	}
	return nil
}

func (d *DXDatabase) Disconnect() (err error) {
	if d.Connected {
		log.Log.Infof("Disconnecting to database %s/%s... start", d.NameId, d.NonSensitiveConnectionString)
		err := (*d.Connection).Close()
		if err != nil {
			log.Log.Errorf("Disconnecting to database %s/%s error (%s)", d.NameId, d.NonSensitiveConnectionString, err.Error())
			return err
		}
		d.Connection = nil
		d.Connected = false
		log.Log.Infof("Disconnecting to database %s/%s... done DISCONNECTED", d.NameId, d.NonSensitiveConnectionString)
	}
	return nil
}

func (d *DXDatabase) Execute(statement string, parameters utils.JSON) (r any, err error) {
	//err = d.CheckConnectionAndReconnect()
	//if err != nil {
	//	return nil, err
	//}
	isDDL := utilsSql.IsDDL(statement)
	if !isDDL {
		query := pq.NewNamedParameterQuery(statement)
		query.SetValuesFromMap(parameters)
		s := query.GetParsedQuery()
		p := query.GetParsedParameters()
		r, err = d.Connection.Exec(s, p...)
		return r, err
	}
	s := statement
	for k, v := range parameters {
		vs := ""
		switch v.(type) {
		case string:
			// for Postgresql is "
			vs = fmt.Sprintf(`"%s"`, v)
		case int, int8, int16, int32, int64:
			vs = strconv.FormatInt(v.(int64), 10)
		case float32, float64:
			vs = fmt.Sprintf("%f", v)
		}
		s = strings.Replace(s, `:`+k, vs, -1)
	}
	r, err = d.Connection.Exec(s)
	return r, err
}

func (d *DXDatabase) PropertyValue(key string) (value string, err error) {
	//err = d.CheckConnectionAndReconnect()
	//if err != nil {
	//	return "", err
	//}
	_, resultData, err := db.MustSelectOne(d.Connection, "properties", nil, utils.JSON{
		"key": key,
	}, nil, nil)
	if err != nil {
		return "", err
	}
	value = resultData["value"].(string)
	return value, nil
}

func (d *DXDatabase) Insert(tableName string, fieldNameForRowId string, keyValues utils.JSON) (id int64, err error) {
	//err = d.CheckConnectionAndReconnect()
	//if err != nil {
	//	return 0, err
	//}
	return db.Insert(d.Connection, tableName, fieldNameForRowId, keyValues)
}

func (d *DXDatabase) Update(tableName string, setKeyValues utils.JSON, whereKeyValues utils.JSON) (result sql.Result, err error) {
	//err = d.CheckConnectionAndReconnect()
	//if err != nil {
	//	return nil, err
	//}
	return db.UpdateWhereKeyValues(d.Connection, tableName, setKeyValues, whereKeyValues)
}

func (d *DXDatabase) MustSelectOne(tableName string, whereAndFieldNameValues utils.JSON, orderbyFieldNameDirections map[string]string) (
	rowsInfo *db.RowsInfo, resultData utils.JSON, err error) {
	//err = d.CheckConnectionAndReconnect()
	//if err != nil {
	//	return nil, nil, err
	//}
	rowsInfo, resultData, err = db.MustSelectOne(d.Connection, tableName, nil, whereAndFieldNameValues, nil, orderbyFieldNameDirections)
	return rowsInfo, resultData, err
}

func (d *DXDatabase) Select(tableName string, showFieldNames []string, whereAndFieldNameValues utils.JSON, orderbyFieldNameDirections map[string]string,
	limit any) (rowsInfo *db.RowsInfo, resultData []utils.JSON, err error) {
	//err = d.CheckConnectionAndReconnect()
	//if err != nil {
	//	return nil, nil, err
	//}
	return db.Select(d.Connection, tableName, showFieldNames, whereAndFieldNameValues, nil, orderbyFieldNameDirections, limit)
}

func (d *DXDatabase) SelectOne(tableName string, fieldNames []string, whereAndFieldNameValues utils.JSON, joinSQLPart any,
	orderbyFieldNameDirections map[string]string) (rowsInfo *db.RowsInfo, r utils.JSON, err error) {

	tryCount := 0
	for {
		rowsInfo, r, err = db.SelectOne(d.Connection, tableName, fieldNames, whereAndFieldNameValues, joinSQLPart, orderbyFieldNameDirections)
		if err == nil {
			return rowsInfo, r, nil
		}
		if err != nil {
			if tryCount < 4 {
				tryCount++
				log.Log.Warnf("SELECT_ONE_ERROR:%s=%v", tableName, err.Error())
				err = d.CheckConnectionAndReconnect()
				if err != nil {
					return nil, nil, err
				}
			}
		}
	}
}

func (d *DXDatabase) ExecuteFile(filename string) (r sql.Result, err error) {
	//err = d.CheckConnectionAndReconnect()
	//if err != nil {
	//	return nil, err
	//}
	/*	log.Log.Infof("Executing SQL file %s... start", filename)
		fs := sqlfile.SqlFile{}
		err = fs.File(filename)
		if err != nil {
			log.Log.Panic("DXDatabaseScript/ExecuteFile/1", err)
			return nil, err
		}
		rs, err := fs.Exec(d.Connection.DB)
		if err != nil {
			log.Log.Fatalf("Error executing SQL file %s (%v)", filename, err.Error())
			return rs[0], err
		}
		log.Log.Infof("Executing SQL file %s... done", filename)
		return rs[0], nil
	*/
	defer func() {
		if err != nil {
			log.Log.Errorf("Error executing file %s (%v)", filename, err.Error())
		}
	}()

	sqlScript, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	// Execute the SQL script
	r, err = d.Connection.Exec(string(sqlScript))
	if err != nil {
		return nil, err
	}

	log.Log.Info("SQL script executed successfully!")
	return r, nil
}

func (d *DXDatabase) ExecuteCreateScripts() (rs []sql.Result, err error) {
	if !d.Connected {
		err = d.Connect()
		if err != nil {
			return nil, err
		}
	}
	rs = []sql.Result{}
	for k, v := range d.CreateScriptFiles {
		r, err := d.ExecuteFile(v)
		if err != nil {
			log.Log.Errorf("Error executing file %d:'%s' (%s)", k, v, err.Error())
			return rs, err
		}
		log.Log.Infof("Executing file %d:'%s'... done", k+1, v)
		rs = append(rs, r)
	}
	return rs, nil
}

func (d *DXDatabase) Tx(log *log.DXLog, isolationLevel sql.IsolationLevel, callback DXDatabaseTxCallback) (err error) {
	//err = d.CheckConnectionAndReconnect()
	//if err != nil {
	//	return err
	//}
	tx, err := d.Connection.BeginTxx(log.Context, &sql.TxOptions{
		Isolation: isolationLevel,
		ReadOnly:  false,
	})
	if err != nil {
		log.Error(err.Error())
		return err
	}
	dtx := &DXDatabaseTx{
		Tx:  tx,
		Log: log,
	}
	err = callback(dtx)
	if err != nil {
		log.Errorf(`TX_ERROR_IN_CALLBACK: (%v)`, err.Error())
		errTx := tx.Rollback()
		if errTx != nil {
			log.Errorf(`SHOULD_NOT_HAPPEN:ERROR_IN_ROLLBACK(%v)`, errTx.Error())
		}
		return err
	}
	err = dtx.Tx.Commit()
	if err != nil {
		log.Errorf(`TX_ERROR_IN_COMMIT: (%v)`, err.Error())
		errTx := tx.Rollback()
		if errTx != nil {
			log.Errorf(`ErrorInCommitRollback: (%v)`, errTx.Error())
		}
		return err
	}

	return nil
}

func (dtx *DXDatabaseTx) SelectOne(tableName string, fieldNames []string, whereAndFieldNameValues utils.JSON, joinSQLPart any,
	orderbyFieldNameDirections map[string]string, forUpdatePart any) (rowsInfo *db.RowsInfo, r utils.JSON, err error) {
	return dbtx.TxSelectOne(dtx.Log, false, dtx.Tx, tableName, fieldNames, whereAndFieldNameValues, joinSQLPart, orderbyFieldNameDirections, forUpdatePart)
}

func (dtx *DXDatabaseTx) MustSelectOne(tableName string, fieldNames []string, whereAndFieldNameValues utils.JSON, joinSQLPart any,
	orderbyFieldNameDirections map[string]string, forUpdatePart any) (rowsInfo *db.RowsInfo, r utils.JSON, err error) {
	return dbtx.TxMustSelectOne(dtx.Log, false, dtx.Tx, tableName, fieldNames, whereAndFieldNameValues, joinSQLPart, orderbyFieldNameDirections, forUpdatePart)
}
func (dtx *DXDatabaseTx) Insert(tableName string, keyValues utils.JSON) (id int64, err error) {
	return dbtx.TxInsert(dtx.Log, false, dtx.Tx, tableName, keyValues)
}

func (dtx *DXDatabaseTx) UpdateOne(tableName string, setKeyValues utils.JSON, whereKeyValues utils.JSON) (result utils.JSON, err error) {
	return dbtx.TxUpdateOne(dtx.Log, false, dtx.Tx, tableName, setKeyValues, whereKeyValues)
}
