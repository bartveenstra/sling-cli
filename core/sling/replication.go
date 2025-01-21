package sling

import (
	"context"
	"database/sql/driver"
	"io"
	"os"
	"strings"

	"github.com/flarco/g"
	"github.com/gobwas/glob"
	"github.com/samber/lo"
	"github.com/slingdata-io/sling-cli/core/dbio/connection"
	"github.com/slingdata-io/sling-cli/core/dbio/database"
	"github.com/slingdata-io/sling-cli/core/dbio/filesys"
	"github.com/slingdata-io/sling-cli/core/dbio/iop"
	"github.com/spf13/cast"
	"gopkg.in/yaml.v2"
)

type ReplicationConfig struct {
	Source   string                              `json:"source,omitempty" yaml:"source,omitempty"`
	Target   string                              `json:"target,omitempty" yaml:"target,omitempty"`
	Hooks    HookMap                             `json:"hooks,omitempty" yaml:"hooks,omitempty"`
	Defaults ReplicationStreamConfig             `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	Streams  map[string]*ReplicationStreamConfig `json:"streams,omitempty" yaml:"streams,omitempty"`
	Env      map[string]any                      `json:"env,omitempty" yaml:"env,omitempty"`

	// Tasks are compiled tasks
	Tasks    []*Config `json:"tasks"`
	Compiled bool      `json:"compiled"`
	FailErr  string    // error string to fail all (e.g. when the first tasks fails to connect)

	streamsOrdered []string
	originalCfg    string
	maps           replicationConfigMaps // raw maps for validation
	state          *RuntimeState
}

type replicationConfigMaps struct {
	Defaults map[string]any
	Streams  map[string]map[string]any
}

// OriginalCfg returns original config
func (rd *ReplicationConfig) OriginalCfg() string {
	return rd.originalCfg
}

// JSON returns json payload
func (rd *ReplicationConfig) JSON() string {
	payload := g.Marshal([]any{
		g.M("source", rd.Source),
		g.M("target", rd.Target),
		g.M("hooks", rd.Hooks),
		g.M("defaults", rd.Defaults),
		g.M("streams", rd.Streams),
		g.M("env", rd.Env),
	})

	// clean up
	if strings.Contains(rd.Source, "://") {
		cleanSource := strings.Split(rd.Source, "://")[0] + "://"
		payload = strings.ReplaceAll(payload, g.Marshal(rd.Source), g.Marshal(cleanSource))
	}

	if strings.Contains(rd.Target, "://") {
		cleanTarget := strings.Split(rd.Target, "://")[0] + "://"
		payload = strings.ReplaceAll(payload, g.Marshal(rd.Target), g.Marshal(cleanTarget))
	}

	return payload
}

// StateMap returns map for use
func (rd *ReplicationConfig) RuntimeState() (_ *RuntimeState, err error) {
	if rd.state == nil {
		rd.state = &RuntimeState{
			Hooks:  map[string]map[string]any{},
			Env:    rd.Env,
			Runs:   map[string]*RunState{},
			Source: ConnState{Name: rd.Source},
			Target: ConnState{Name: rd.Target},
		}
	}

	rd.state.Timestamp.Update()

	if rd.Compiled {
		for _, task := range rd.Tasks {
			fMap, err := task.GetFormatMap()
			if err != nil {
				return rd.state, err
			}

			// populate source
			rd.state.Source.Type = task.SrcConn.Type
			rd.state.Source.Kind = task.SrcConn.Type.Kind()
			rd.state.Source.Bucket = cast.ToString(fMap["source_bucket"])
			rd.state.Source.Container = cast.ToString(fMap["source_container"])
			rd.state.Source.Database = cast.ToString(task.SrcConn.Data["database"])
			rd.state.Source.Instance = cast.ToString(task.SrcConn.Data["instance"])
			rd.state.Source.Schema = cast.ToString(task.SrcConn.Data["schema"])

			// populate target
			rd.state.Target.Type = task.TgtConn.Type
			rd.state.Target.Kind = task.TgtConn.Type.Kind()
			rd.state.Target.Bucket = cast.ToString(fMap["target_bucket"])
			rd.state.Target.Container = cast.ToString(fMap["target_container"])
			rd.state.Target.Database = cast.ToString(task.TgtConn.Data["database"])
			rd.state.Target.Instance = cast.ToString(task.TgtConn.Data["instance"])
			rd.state.Target.Schema = cast.ToString(task.TgtConn.Data["schema"])

			runID := iop.CleanName(rd.Normalize(task.StreamName))
			if id := cast.ToString(fMap["stream_run_id"]); id != "" {
				runID = id
			}

			if _, ok := rd.state.Runs[runID]; !ok {
				rd.state.Runs[runID] = &RunState{
					ID:     runID,
					Status: ExecStatusCreated,
					Stream: &StreamState{
						FileFolder: cast.ToString(fMap["stream_file_folder"]),
						FileName:   cast.ToString(fMap["stream_file_name"]),
						FileExt:    cast.ToString(fMap["stream_file_ext"]),
						FilePath:   cast.ToString(fMap["stream_file_path"]),
						Name:       cast.ToString(fMap["stream_name"]),
						Schema:     cast.ToString(fMap["stream_schema"]),
						Table:      cast.ToString(fMap["stream_table"]),
						FullName:   cast.ToString(fMap["stream_full_name"]),
					},
					Object: &ObjectState{
						Name:     cast.ToString(fMap["object_name"]),
						Schema:   cast.ToString(fMap["object_schema"]),
						Table:    cast.ToString(fMap["object_table"]),
						FullName: cast.ToString(fMap["object_full_name"]),
					},
				}
			}

		}
	}

	return rd.state, nil
}

// MD5 returns a md5 hash of the json payload
func (rd *ReplicationConfig) MD5() string {
	return g.MD5(rd.OriginalCfg())
}

// Scan scan value into Jsonb, implements sql.Scanner interface
func (rd *ReplicationConfig) Scan(value interface{}) error {
	return g.JSONScanner(rd, value)
}

// Value return json value, implement driver.Valuer interface
func (rd ReplicationConfig) Value() (driver.Value, error) {
	return []byte(rd.JSON()), nil
}

// StreamsOrdered returns the stream names as ordered in the YAML file
func (rd ReplicationConfig) StreamsOrdered() []string {
	return rd.streamsOrdered
}

// GetStream returns the stream if the it exists
func (rd ReplicationConfig) GetStream(name string) (streamName string, cfg *ReplicationStreamConfig, found bool) {

	for streamName, streamCfg := range rd.Streams {
		if rd.Normalize(streamName) == rd.Normalize(name) {
			return streamName, streamCfg, true
		}
	}
	return
}

// GetStream returns the stream if the it exists
func (rd ReplicationConfig) MatchStreams(pattern string) (streams map[string]*ReplicationStreamConfig) {
	streams = map[string]*ReplicationStreamConfig{}
	gc, err := glob.Compile(strings.ToLower(pattern))
	for streamName, streamCfg := range rd.Streams {
		if rd.Normalize(streamName) == rd.Normalize(pattern) {
			streams[streamName] = streamCfg
		} else if streamCfg.ID == pattern {
			streams[streamName] = streamCfg
		} else if err == nil && gc.Match(strings.ToLower(rd.Normalize(streamName))) {
			streams[streamName] = streamCfg
		}
	}
	return
}

// Normalize normalized the name
func (rd ReplicationConfig) Normalize(n string) string {
	n = strings.ReplaceAll(n, "`", "")
	n = strings.ReplaceAll(n, `"`, "")
	n = strings.ToLower(n)
	return n
}

type Wildcards []*Wildcard

func (ws Wildcards) Patterns() []string {
	patterns := []string{}
	for _, w := range ws {
		patterns = append(patterns, w.Pattern)
	}
	return patterns
}

type Wildcard struct {
	Pattern     string
	StreamNames []string
	NodeMap     map[string]filesys.FileNode
	TableMap    map[string]database.Table
}

// ProcessWildcards process the streams using wildcards
// such as `my_schema.*` or `my_schema.my_prefix_*` or `my_schema.*_my_suffix`
func (rd *ReplicationConfig) ProcessWildcards() (err error) {
	hasWildcard := func(name string) bool {
		return strings.Contains(name, "*") || strings.Contains(name, "?")
	}

	patterns := []string{}
	for _, name := range rd.streamsOrdered {
		// if specified, treat wildcard as single stream (don't expand wildcard into individual streams), will be expand while reading
		stream := rd.Streams[name]
		if stream != nil && stream.Single != nil {
			if *stream.Single {
				continue
			}
		} else if rd.Defaults.Single != nil && *rd.Defaults.Single {
			continue
		}

		if name == "*" {
			return g.Error("Must specify schema or path when using wildcard: 'my_schema.*', 'file://./my_folder/*', not '*'")
		} else if hasWildcard(name) {
			// use a clone stream to apply defaults
			s := ReplicationStreamConfig{}
			if stream != nil {
				s = *stream
			}

			// apply default
			SetStreamDefaults(name, &s, *rd)

			// if the target object doesn't have runtime variables, consider as single
			if !s.ObjectHasStreamVars() {
				// if file max vars are zero as well for file targets, auto-set as single
				value := g.PtrVal(g.PtrVal(stream.TargetOptions).FileMaxBytes) == 0 && g.PtrVal(g.PtrVal(stream.TargetOptions).FileMaxRows) == 0
				stream.Single = g.Ptr(value)
				continue
			}
			patterns = append(patterns, name)
		}
	}
	if len(patterns) == 0 {
		return
	}

	// get local connections
	connsMap := lo.KeyBy(connection.GetLocalConns(), func(c connection.ConnEntry) string {
		return strings.ToLower(c.Connection.Name)
	})
	c, ok := connsMap[strings.ToLower(rd.Source)]
	if !ok {
		if strings.EqualFold(rd.Source, "local://") || strings.EqualFold(rd.Source, "file://") {
			c = connection.LocalFileConnEntry()
		} else if strings.Contains(rd.Source, "://") {
			c.Connection, err = connection.NewConnectionFromURL("source", rd.Source)
			if err != nil {
				return
			}
		} else {
			g.Error("did not find connection for wildcards: %s", rd.Source)
			return
		}
	}

	originalStreamNames := rd.streamsOrdered
	originalNormalizedStreamNames := map[string]string{}
	for _, name := range originalStreamNames {
		originalNormalizedStreamNames[rd.Normalize(name)] = name
	}

	var wildcards Wildcards
	if c.Connection.Type.IsDb() {
		if wildcards, err = rd.ProcessWildcardsDatabase(c.Connection, patterns); err != nil {
			return err
		}
	} else if c.Connection.Type.IsFile() {
		if wildcards, err = rd.ProcessWildcardsFile(c.Connection, patterns); err != nil {
			return err
		}
	} else {
		return g.Error("invalid connection for wildcards: %s", rd.Source)
	}

	// add wildcard streams
	// arrange order to reflect original
	newStreamNames := []string{}
	for _, origName := range originalStreamNames {
		matched := false
		for _, wildcard := range wildcards {
			if wildcard.Pattern == origName {
				matched = true
				for _, wsn := range wildcard.StreamNames {
					if c.Connection.Type.IsDb() {
						table := wildcard.TableMap[wsn]

						// check if table name exists
						_, _, found := rd.GetStream(table.FullName())
						if found {
							// leave as is for order to be respected
							continue
						}

						cfg := rd.Streams[wildcard.Pattern]
						rd.AddStream(table.FullName(), cfg)
						newStreamNames = append(newStreamNames, table.FullName())
					}

					if c.Connection.Type.IsFile() {
						node := wildcard.NodeMap[wsn]

						// check if node path exists
						_, _, found := rd.GetStream(node.Path())
						if found {
							// leave as is for order to be respected
							continue
						}

						// check if node URI exists
						_, _, found = rd.GetStream(node.URI)
						if found {
							// leave as is for order to be respected
							continue
						}

						cfg := rd.Streams[wildcard.Pattern]
						rd.AddStream(node.Path(), cfg)
						newStreamNames = append(newStreamNames, node.Path())
					}
				}

				// remove original pattern stream
				rd.DeleteStream(wildcard.Pattern)
			}
		}

		if !matched {
			newStreamNames = append(newStreamNames, origName)
		}
	}

	rd.streamsOrdered = newStreamNames

	return nil
}

func (rd *ReplicationConfig) ParseReplicationHook(stage HookStage) (hooks Hooks, err error) {
	var hooksRaw []any
	switch stage {
	case HookStageStart:
		hooksRaw = rd.Hooks.Start
	case HookStageEnd:
		hooksRaw = rd.Hooks.End
	default:
		return nil, g.Error("invalid default hook stage: %s", stage)
	}

	if hooksRaw == nil {
		return
	}

	state, err := rd.RuntimeState()
	if err != nil {
		return nil, g.Error(err, "could not render runtime state")
	}

	for i, hook := range hooksRaw {
		opts := ParseOptions{stage: stage, index: i, state: state}
		hook, err := ParseHook(hook, opts)
		if err != nil {
			return nil, g.Error(err, "error parsing %s-hook", stage)
		} else if hook != nil {
			hooks = append(hooks, hook)
		}
	}
	return hooks, nil
}

func (rd *ReplicationConfig) ParseStreamHook(stage HookStage, rs *ReplicationStreamConfig) (hooks Hooks, err error) {
	var hooksRaw []any
	switch stage {
	case HookStagePre:
		hooksRaw = rs.Hooks.Pre
	case HookStagePost:
		hooksRaw = rs.Hooks.Post
	default:
		return nil, g.Error("invalid stream hook stage: %s", stage)
	}

	if hooksRaw == nil {
		return
	}

	for i, hook := range hooksRaw {
		opts := ParseOptions{stage: stage, index: i, state: rd.state}
		hook, err := ParseHook(hook, opts)
		if err != nil {
			return nil, g.Error(err, "error parsing %s-hook", stage)
		} else if hook != nil {
			hooks = append(hooks, hook)
		}
	}
	return hooks, nil
}

func (rd *ReplicationConfig) AddStream(key string, cfg *ReplicationStreamConfig) {
	newCfg := ReplicationStreamConfig{}
	g.Unmarshal(g.Marshal(cfg), &newCfg) // copy config over
	rd.Streams[key] = &newCfg
	rd.streamsOrdered = append(rd.streamsOrdered, key)

	// add to streams map if not found
	if _, found := rd.maps.Streams[key]; !found {
		mapEntry, _ := g.UnmarshalMap(g.Marshal(cfg))
		rd.maps.Streams[key] = mapEntry
	}
}

func (rd *ReplicationConfig) DeleteStream(key string) {
	delete(rd.Streams, key)
	rd.streamsOrdered = lo.Filter(rd.streamsOrdered, func(v string, i int) bool {
		return v != key
	})
}

func (rd *ReplicationConfig) ProcessWildcardsDatabase(c connection.Connection, patterns []string) (wildcards Wildcards, err error) {

	g.DebugLow("processing wildcards for %s: %s", rd.Source, g.Marshal(patterns))

	conn, err := c.AsDatabase(true)
	if err != nil {
		return wildcards, g.Error(err, "could not init connection for wildcard processing: %s", rd.Source)
	} else if err = conn.Connect(); err != nil {
		return wildcards, g.Error(err, "could not connect to database for wildcard processing: %s", rd.Source)
	}

	for _, pattern := range patterns {
		wildcard := Wildcard{Pattern: pattern, TableMap: map[string]database.Table{}}

		schemaT, err := database.ParseTableName(pattern, c.Type)
		if err != nil {
			return wildcards, g.Error(err, "could not parse stream name: %s", pattern)
		} else if schemaT.Schema == "" {
			continue
		}

		// get all tables in schema
		g.Debug("getting tables for %s", pattern)
		ok, _, schemata, err := c.Discover(&connection.DiscoverOptions{Pattern: pattern})
		if err != nil {
			return wildcards, g.Error(err, "could not get tables for schema: %s", schemaT.Schema)
		} else if !ok {
			return wildcards, g.Error("could not get tables for schema: %s", schemaT.Schema)
		}

		for _, table := range schemata.Tables() {
			wildcard.StreamNames = append(wildcard.StreamNames, table.FullName())
			wildcard.TableMap[table.FullName()] = table
		}

		g.Debug("wildcard '%s' matched %d streams => %+v", pattern, len(wildcard.StreamNames), wildcard.StreamNames)

		// delete * from stream map
		wildcards = append(wildcards, &wildcard)

	}
	return
}

func (rd *ReplicationConfig) ProcessWildcardsFile(c connection.Connection, patterns []string) (wildcards Wildcards, err error) {
	g.DebugLow("processing wildcards for %s: %s", rd.Source, g.Marshal(patterns))

	fs, err := c.AsFile(true)
	if err != nil {
		return wildcards, g.Error(err, "could not init connection for wildcard processing: %s", rd.Source)
	} else if err = fs.Init(context.Background()); err != nil {
		return wildcards, g.Error(err, "could not connect to file system for wildcard processing: %s", rd.Source)
	}

	for _, pattern := range patterns {
		path := pattern

		wildcard := Wildcard{Pattern: pattern, NodeMap: map[string]filesys.FileNode{}}
		if strings.Contains(pattern, "://") {
			_, _, path, err = filesys.ParseURLType(pattern)
			if err != nil {
				return wildcards, g.Error(err, "could not parse wildcard: %s", pattern)
			}
		}

		ok, nodes, _, err := c.Discover(&connection.DiscoverOptions{Pattern: path})
		if err != nil {
			return wildcards, g.Error(err, "could not get files for schema: %s", pattern)
		} else if !ok {
			return wildcards, g.Error("could not get files for schema: %s", pattern)
		}

		for _, node := range nodes {
			// add path
			wildcard.StreamNames = append(wildcard.StreamNames, node.Path())
			wildcard.NodeMap[node.Path()] = node
		}

		g.Debug("wildcard '%s' matched %d streams => %+v", pattern, len(wildcard.StreamNames), wildcard.StreamNames)

		// delete from stream map
		wildcards = append(wildcards, &wildcard)

	}

	return
}

// Compile compiles the replication into tasks
func (rd *ReplicationConfig) Compile(cfgOverwrite *Config, selectStreams ...string) (err error) {
	if rd.Compiled {
		// apply the selection if specified
		if len(selectStreams) > 0 {
			selectedTasks := []*Config{}
			nameMap := g.ArrMapString(selectStreams)
			for _, task := range rd.Tasks {
				if _, ok := nameMap[task.StreamName]; ok {
					selectedTasks = append(selectedTasks, task)
				}
			}
			rd.Tasks = selectedTasks
		}
		return nil
	}

	err = rd.ProcessWildcards()
	if err != nil {
		return g.Error(err, "could not process streams using wildcard")
	}

	// clean up selectStreams
	matchedStreams := map[string]*ReplicationStreamConfig{}
	includeTags := []string{}
	excludeTags := []string{}
	for _, selectStream := range selectStreams {
		for key, val := range rd.MatchStreams(selectStream) {
			key = rd.Normalize(key)
			matchedStreams[key] = val
		}
		if strings.HasPrefix(selectStream, "tag:") {
			includeTags = append(includeTags, strings.TrimPrefix(selectStream, "tag:"))
		}
		if strings.HasPrefix(selectStream, "-tag:") {
			excludeTags = append(excludeTags, strings.TrimPrefix(selectStream, "-tag:"))
		}
	}

	if len(includeTags) > 0 && len(excludeTags) > 0 {
		return g.Error("cannot include and exclude tags. Either include or exclude.")
	}

	for _, name := range rd.StreamsOrdered() {

		stream := ReplicationStreamConfig{}
		if rd.Streams[name] != nil {
			stream = *rd.Streams[name]
		}
		SetStreamDefaults(name, &stream, *rd)
		stream.replication = rd

		if stream.Object == "" {
			return g.Error("need to specify `object` for stream `%s`. Please see https://docs.slingdata.io/sling-cli for help.", name)
		}

		// match on tag, need stream defined to do so
		matchedTag := false
		for _, tag := range includeTags {
			if g.In(tag, stream.Tags...) {
				matchedTag = true
			}
		}
		if matchedTag {
			matchedStreams[rd.Normalize(name)] = &stream
		}

		// exclude tags
		for _, tag := range excludeTags {
			if g.In(tag, stream.Tags...) {
				delete(matchedStreams, rd.Normalize(name))
			}
		}

		_, matched := matchedStreams[rd.Normalize(name)]
		if len(selectStreams) > 0 && !matched {
			g.Trace("skipping stream %s since it is not selected", name)
			continue
		}

		// config overwrite
		taskEnv := g.ToMapString(rd.Env)
		var incrementalValStr string

		if cfgOverwrite != nil {
			if string(cfgOverwrite.Mode) != "" && stream.Mode != cfgOverwrite.Mode {
				g.Debug("stream mode overwritten for `%s`: %s => %s", name, stream.Mode, cfgOverwrite.Mode)
				stream.Mode = cfgOverwrite.Mode
			}

			if cfgOverwrite.Source.Options.Limit != nil && stream.SourceOptions.Limit != cfgOverwrite.Source.Options.Limit {
				if stream.SourceOptions.Limit != nil {
					g.Debug("stream limit overwritten for `%s`: %s => %s", name, *stream.SourceOptions.Limit, *cfgOverwrite.Source.Options.Limit)
				}
				stream.SourceOptions.Limit = cfgOverwrite.Source.Options.Limit
			}

			if cfgOverwrite.Source.Options.Offset != nil && stream.SourceOptions.Offset != cfgOverwrite.Source.Options.Offset {
				if stream.SourceOptions.Offset != nil {
					g.Debug("stream offset overwritten for `%s`: %s => %s", name, *stream.SourceOptions.Offset, *cfgOverwrite.Source.Options.Offset)
				}
				stream.SourceOptions.Offset = cfgOverwrite.Source.Options.Offset
			}

			if cfgOverwrite.Source.UpdateKey != "" && stream.UpdateKey != cfgOverwrite.Source.UpdateKey {
				if stream.UpdateKey != "" {
					g.Debug("stream update_key overwritten for `%s`: %s => %s", name, stream.UpdateKey, cfgOverwrite.Source.UpdateKey)
				}
				stream.UpdateKey = cfgOverwrite.Source.UpdateKey
			}

			if cfgOverwrite.Source.PrimaryKeyI != nil && stream.PrimaryKeyI != cfgOverwrite.Source.PrimaryKeyI {
				if stream.PrimaryKeyI != nil {
					g.Debug("stream primary_key overwritten for `%s`: %#v => %#v", name, stream.PrimaryKeyI, cfgOverwrite.Source.PrimaryKeyI)
				}
				stream.PrimaryKeyI = cfgOverwrite.Source.PrimaryKeyI
			}

			if newRange := cfgOverwrite.Source.Options.Range; newRange != nil {
				if stream.SourceOptions.Range == nil || *stream.SourceOptions.Range != *newRange {
					if stream.SourceOptions.Range != nil && *stream.SourceOptions.Range != "" {
						g.Debug("stream range overwritten for `%s`: %s => %s", name, *stream.SourceOptions.Range, *newRange)
					}
					stream.SourceOptions.Range = newRange
				}
			}

			// other incremental / backfill overrides
			if newFileSelect := cfgOverwrite.Source.Options.FileSelect; newFileSelect != nil {
				stream.SourceOptions.FileSelect = newFileSelect
			}
			incrementalValStr = cfgOverwrite.IncrementalValStr

			// merge to existing replication env, overwrite if key already exists
			if cfgOverwrite.Env != nil {
				for k, v := range cfgOverwrite.Env {
					taskEnv[k] = v
				}
			}
		}

		cfg := Config{
			Source: Source{
				Conn:        rd.Source,
				Stream:      name,
				Query:       stream.SQL,
				Select:      stream.Select,
				Where:       stream.Where,
				PrimaryKeyI: stream.PrimaryKey(),
				UpdateKey:   stream.UpdateKey,
			},
			Target: Target{
				Conn:    rd.Target,
				Object:  stream.Object,
				Columns: stream.Columns,
			},
			Mode:              stream.Mode,
			Transforms:        stream.Transforms,
			Env:               taskEnv,
			StreamName:        name,
			IncrementalValStr: incrementalValStr,
			ReplicationStream: &stream,
		}

		// so that the next stream does not retain previous pointer values
		g.Unmarshal(g.Marshal(stream.SourceOptions), &cfg.Source.Options)
		g.Unmarshal(g.Marshal(stream.TargetOptions), &cfg.Target.Options)

		// if single file target, set file_row_limit and file_bytes_limit
		if stream.Single != nil && *stream.Single {
			if cfg.Target.Options == nil {
				cfg.Target.Options = &TargetOptions{}
			}
			cfg.Target.Options.FileMaxBytes = g.Int64(0)
			cfg.Target.Options.FileMaxRows = g.Int64(0)
		}

		// prepare config
		err = cfg.Prepare()
		if err != nil {
			err = g.Error(err, "could not prepare stream task: %s", name)
			return
		}

		rd.Tasks = append(rd.Tasks, &cfg)
	}

	rd.Compiled = true

	// generate state
	if _, err = rd.RuntimeState(); err != nil {
		return g.Error(err, "could not make runtime state")
	}

	g.Trace("len(selectStreams) = %d, len(matchedStreams) = %d, len(replication.Streams) = %d", len(selectStreams), len(matchedStreams), len(rd.Streams))
	streamCnt := lo.Ternary(len(selectStreams) > 0, len(matchedStreams), len(rd.Streams))

	if err = testStreamCnt(streamCnt, lo.Keys(matchedStreams), lo.Keys(rd.Streams)); err != nil {
		return err
	}
	return
}

type ReplicationStreamConfig struct {
	ID            string         `json:"id,omitempty" yaml:"id,omitempty"`
	Description   string         `json:"description,omitempty" yaml:"description,omitempty"`
	Mode          Mode           `json:"mode,omitempty" yaml:"mode,omitempty"`
	Object        string         `json:"object,omitempty" yaml:"object,omitempty"`
	Select        []string       `json:"select,omitempty" yaml:"select,flow,omitempty"`
	Where         string         `json:"where,omitempty" yaml:"where,omitempty"`
	PrimaryKeyI   any            `json:"primary_key,omitempty" yaml:"primary_key,flow,omitempty"`
	UpdateKey     string         `json:"update_key,omitempty" yaml:"update_key,omitempty"`
	SQL           string         `json:"sql,omitempty" yaml:"sql,omitempty"`
	Tags          []string       `json:"tags,omitempty" yaml:"tags,omitempty"`
	SourceOptions *SourceOptions `json:"source_options,omitempty" yaml:"source_options,omitempty"`
	TargetOptions *TargetOptions `json:"target_options,omitempty" yaml:"target_options,omitempty"`
	Schedule      string         `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Disabled      bool           `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	Single        *bool          `json:"single,omitempty" yaml:"single,omitempty"`
	Transforms    any            `json:"transforms,omitempty" yaml:"transforms,omitempty"`
	Columns       any            `json:"columns,omitempty" yaml:"columns,omitempty"`
	Hooks         HookMap        `json:"hooks,omitempty" yaml:"hooks,omitempty"`

	replication *ReplicationConfig `json:"-" yaml:"-"`
}

func (s *ReplicationStreamConfig) PrimaryKey() []string {
	return castKeyArray(s.PrimaryKeyI)
}

func (s *ReplicationStreamConfig) ObjectHasStreamVars() bool {
	vars := []string{
		"stream_table",
		"stream_name",
		"stream_file_path",
		"stream_file_name",
	}
	for _, v := range vars {
		if strings.Contains(s.Object, g.F("{%s}", v)) {
			return true
		}
	}
	return false
}

func SetStreamDefaults(name string, stream *ReplicationStreamConfig, replicationCfg ReplicationConfig) {

	streamMap, ok := replicationCfg.maps.Streams[name]
	if !ok {
		streamMap = g.M()
	}

	// the keys to check if provided in map
	defaultSet := map[string]func(){
		"mode":        func() { stream.Mode = replicationCfg.Defaults.Mode },
		"object":      func() { stream.Object = replicationCfg.Defaults.Object },
		"select":      func() { stream.Select = replicationCfg.Defaults.Select },
		"where":       func() { stream.Where = replicationCfg.Defaults.Where },
		"primary_key": func() { stream.PrimaryKeyI = replicationCfg.Defaults.PrimaryKeyI },
		"update_key":  func() { stream.UpdateKey = replicationCfg.Defaults.UpdateKey },
		"sql":         func() { stream.SQL = replicationCfg.Defaults.SQL },
		"schedule":    func() { stream.Schedule = replicationCfg.Defaults.Schedule },
		"tags":        func() { stream.Tags = replicationCfg.Defaults.Tags },
		"disabled":    func() { stream.Disabled = replicationCfg.Defaults.Disabled },
		"single":      func() { stream.Single = g.Ptr(g.PtrVal(replicationCfg.Defaults.Single)) },
		"transforms":  func() { stream.Transforms = replicationCfg.Defaults.Transforms },
		"columns":     func() { stream.Columns = replicationCfg.Defaults.Columns },
		"hooks":       func() { stream.Hooks = g.PtrVal(g.Ptr(replicationCfg.Defaults.Hooks)) },
	}

	for key, setFunc := range defaultSet {
		if _, found := streamMap[key]; !found {
			setFunc() // if not found, set default
		}
	}

	// set default options
	if stream.SourceOptions == nil {
		stream.SourceOptions = g.Ptr(g.PtrVal(replicationCfg.Defaults.SourceOptions))
	} else if replicationCfg.Defaults.SourceOptions != nil {
		stream.SourceOptions.SetDefaults(*replicationCfg.Defaults.SourceOptions)
	}

	if stream.TargetOptions == nil {
		stream.TargetOptions = g.Ptr(g.PtrVal(replicationCfg.Defaults.TargetOptions))
	} else if replicationCfg.Defaults.TargetOptions != nil {
		stream.TargetOptions.SetDefaults(*replicationCfg.Defaults.TargetOptions)
	}
}

// UnmarshalReplication converts a yaml file to a replication
func UnmarshalReplication(replicYAML string) (config ReplicationConfig, err error) {

	// set base values when erroring
	config.originalCfg = replicYAML
	config.Env = map[string]any{}

	m := g.M()
	err = yaml.Unmarshal([]byte(replicYAML), &m)
	if err != nil {
		err = g.Error(err, "Error parsing yaml content")
		return
	}

	// parse env & expand variables
	var Env map[string]any
	g.Unmarshal(g.Marshal(m["env"]), &Env)
	for k, v := range Env {
		if s, ok := v.(string); ok {
			Env[k] = os.ExpandEnv(s)
		}
	}

	// replace variables across the yaml file
	Env = lo.Ternary(Env == nil, map[string]any{}, Env)
	replicYAML = g.Rm(replicYAML, Env)

	// parse again
	m = g.M()
	err = yaml.Unmarshal([]byte(replicYAML), &m)
	if err != nil {
		err = g.Error(err, "Error parsing yaml content")
		return
	}

	// source and target
	source, ok := m["source"]
	if !ok {
		err = g.Error("did not find 'source' key")
		return
	}

	target, ok := m["target"]
	if !ok {
		err = g.Error("did not find 'target' key")
		return
	}

	defaults, ok := m["defaults"]
	if !ok {
		defaults = g.M() // defaults not mandatory
	}

	hooks, ok := m["hooks"]
	if !ok {
		hooks = HookMap{} // hooks not mandatory
	}

	streams, ok := m["streams"]
	if !ok {
		err = g.Error("did not find 'streams' key")
		return
	}

	maps := replicationConfigMaps{}
	g.Unmarshal(g.Marshal(defaults), &maps.Defaults)
	g.Unmarshal(g.Marshal(streams), &maps.Streams)

	config = ReplicationConfig{
		Source:      cast.ToString(source),
		Target:      cast.ToString(target),
		Env:         Env,
		maps:        maps,
		originalCfg: replicYAML, // set originalCfg
	}

	// parse defaults
	err = g.Unmarshal(g.Marshal(defaults), &config.Defaults)
	if err != nil {
		err = g.Error(err, "could not parse 'defaults'")
		return
	}

	// parse streams
	err = g.Unmarshal(g.Marshal(streams), &config.Streams)
	if err != nil {
		err = g.Error(err, "could not parse 'streams'")
		return
	}

	// parse hooks
	err = g.Unmarshal(g.Marshal(hooks), &config.Hooks)
	if err != nil {
		err = g.Error(err, "could not parse 'hooks'")
		return
	}

	// get streams & columns order
	rootMap := yaml.MapSlice{}
	err = yaml.Unmarshal([]byte(replicYAML), &rootMap)
	if err != nil {
		err = g.Error(err, "Error parsing yaml content")
		return
	}

	for _, rootNode := range rootMap {
		if cast.ToString(rootNode.Key) == "defaults" {

			if value, ok := rootNode.Value.(yaml.MapSlice); ok {
				if cols := makeColumns(value); cols != nil {
					config.Defaults.Columns = cols
				}
			}

			for _, defaultsNode := range rootNode.Value.(yaml.MapSlice) {
				if cast.ToString(defaultsNode.Key) == "source_options" {
					if value, ok := defaultsNode.Value.(yaml.MapSlice); ok {
						if cols := makeColumns(value); cols != nil {
							config.Defaults.SourceOptions.Columns = cols // legacy
						}
					}
				}
			}
		}
	}

	for _, rootNode := range rootMap {
		if cast.ToString(rootNode.Key) == "streams" {
			streamsNodes, ok := rootNode.Value.(yaml.MapSlice)
			if !ok {
				continue
			}

			for _, streamsNode := range streamsNodes {
				key := cast.ToString(streamsNode.Key)
				stream, found := config.Streams[key]

				config.streamsOrdered = append(config.streamsOrdered, key)
				if streamsNode.Value == nil {
					continue
				}

				if value, ok := streamsNode.Value.(yaml.MapSlice); ok {
					if cols := makeColumns(value); cols != nil {
						stream.Columns = cols
					}
				}

				for _, streamConfigNode := range streamsNode.Value.(yaml.MapSlice) {
					if cast.ToString(streamConfigNode.Key) == "source_options" {
						if found {
							if stream.SourceOptions == nil {
								g.Unmarshal(g.Marshal(config.Defaults.SourceOptions), stream.SourceOptions)
							}
							if value, ok := streamConfigNode.Value.(yaml.MapSlice); ok {
								if cols := makeColumns(value); cols != nil {
									stream.SourceOptions.Columns = cols // legacy
								}
							}
						}
					}
				}
			}
		}
	}

	return
}

// sets the columns correctly and keep the order
func makeColumns(nodes yaml.MapSlice) (columns []any) {
	found := false
	for _, node := range nodes {
		if cast.ToString(node.Key) == "columns" {
			found = true
			if slice, ok := node.Value.(yaml.MapSlice); ok {
				for _, columnNode := range slice {
					col := g.M("name", cast.ToString(columnNode.Key), "type", cast.ToString(columnNode.Value))
					columns = append(columns, col)
				}
			}
		}
	}

	if !found {
		return nil
	}

	return columns
}

func LoadReplicationConfigFromFile(cfgPath string) (config ReplicationConfig, err error) {
	cfgFile, err := os.Open(cfgPath)
	if err != nil {
		err = g.Error(err, "Unable to open replication path: "+cfgPath)
		return
	}

	cfgBytes, err := io.ReadAll(cfgFile)
	if err != nil {
		err = g.Error(err, "could not read from replication path: "+cfgPath)
		return
	}

	config, err = LoadReplicationConfig(string(cfgBytes))
	if err != nil {
		return
	}

	// set config path
	config.Env["SLING_CONFIG_PATH"] = cfgPath

	return
}

func LoadReplicationConfig(content string) (config ReplicationConfig, err error) {
	config, err = UnmarshalReplication(content)
	if err != nil {
		err = g.Error(err, "Error parsing replication config")
		return
	}

	// load compiled tasks if in env
	if payload := os.Getenv("SLING_REPLICATION_TASKS"); payload != "" {
		if err = g.Unmarshal(payload, &config.Tasks); err != nil {
			err = g.Error(err, "could not unmarshal replication compiled tasks")
			return
		}
		config.Compiled = true
	}

	return
}

// IsJSONorYAML detects a JSON or YAML payload
func IsJSONorYAML(payload string) bool {
	if strings.HasPrefix(payload, "{") && strings.HasSuffix(payload, "}") {
		return true
	}
	if strings.Contains(payload, ":") && strings.Contains(payload, "\n") &&
		(strings.Contains(payload, "'") || strings.Contains(payload, `"`)) {
		return true
	}
	return false
}

func testStreamCnt(streamCnt int, matchedStreams, inputStreams []string) error {
	if expected := os.Getenv("SLING_STREAM_CNT"); expected != "" {

		if strings.HasPrefix(expected, ">") {
			atLeast := cast.ToInt(strings.TrimPrefix(expected, ">"))
			if streamCnt <= atLeast {
				return g.Error("Expected at least %d streams, got %d => %s", atLeast, streamCnt, g.Marshal(append(matchedStreams, inputStreams...)))
			}
			return nil
		}

		if streamCnt != cast.ToInt(expected) {
			return g.Error("Expected %d streams, got %d => %s", cast.ToInt(expected), streamCnt, g.Marshal(append(matchedStreams, inputStreams...)))
		}
	}
	return nil
}
