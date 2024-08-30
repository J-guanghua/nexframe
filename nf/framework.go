package nf

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-openapi/spec"
	"github.com/gorilla/mux"
	"github.com/sagoo-cloud/nexframe/nf/g"
	"github.com/sagoo-cloud/nexframe/utils/convert"
	"github.com/sagoo-cloud/nexframe/utils/meta"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// contextKey 是用于存储自定义值的键的类型
type contextKey string

// APIDefinition 定义API结构
type APIDefinition struct {
	HandlerName  string
	RequestType  reflect.Type
	ResponseType reflect.Type
	Meta         meta.Meta
}

// Controller 接口定义控制器的基本结构
type Controller interface {
	// 可以添加通用方法如果需要
}

// APIFramework 核心框架结构
type APIFramework struct {
	addr           string
	router         *mux.Router
	definitions    map[string]APIDefinition
	controllers    map[string]Controller
	weaverServices map[string]interface{}
	prefixes       map[string]string
	middlewares    []mux.MiddlewareFunc
	staticDir      string
	wwwRoot        string
	fileSystem     fs.FS
	debug          bool
	initialized    bool
	initOnce       sync.Once
	contextValues  map[contextKey]interface{}
	contextMu      sync.RWMutex
	swaggerSpec    *spec.Swagger
}

// NewAPIFramework 创建新的APIFramework实例
func NewAPIFramework() *APIFramework {
	return &APIFramework{
		router:         mux.NewRouter(),
		definitions:    make(map[string]APIDefinition),
		controllers:    make(map[string]Controller),
		weaverServices: make(map[string]interface{}),
		prefixes:       make(map[string]string),
		middlewares:    []mux.MiddlewareFunc{},
		debug:          false,
		initialized:    false,
		initOnce:       sync.Once{},
		contextValues:  make(map[contextKey]interface{}),
		swaggerSpec: &spec.Swagger{
			SwaggerProps: spec.SwaggerProps{
				Swagger: "2.0",
				Info: &spec.Info{
					InfoProps: spec.InfoProps{
						Title:       "API Documentation",
						Description: "API documentation generated by the framework",
						Version:     "1.0.0",
					},
				},
				Paths: &spec.Paths{
					Paths: make(map[string]spec.PathItem),
				},
			},
		},
	}
}

// SetContextValue 设置全局上下文值
func (f *APIFramework) SetContextValue(key string, value interface{}) {
	f.contextMu.Lock()
	defer f.contextMu.Unlock()
	f.contextValues[contextKey(key)] = value
}

// GetContextValue 辅助函数，用于在控制器中获取上下文值
func GetContextValue(ctx context.Context, key string) (interface{}, bool) {
	value := ctx.Value(contextKey(key))
	return value, value != nil
}

// createContextMiddleware 创建注入自定义值的中间件
func (f *APIFramework) createContextMiddleware() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			f.contextMu.RLock()
			for k, v := range f.contextValues {
				ctx = context.WithValue(ctx, k, v)
			}
			f.contextMu.RUnlock()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// EnableDebug 启用调试模式
func (f *APIFramework) EnableDebug() *APIFramework {
	f.debug = true
	return f
}

// WithMiddleware 添加一个或多个中间件
func (f *APIFramework) WithMiddleware(middlewares ...mux.MiddlewareFunc) *APIFramework {
	for _, middleware := range middlewares {
		f.middlewares = append(f.middlewares, middleware)
		if f.debug {
			log.Printf("Added middleware: %T\n", middleware)
		}
	}
	return f
}

// SetStaticDir 设置静态资源目录
func (f *APIFramework) SetStaticDir(dir string) *APIFramework {
	f.staticDir = dir
	return f
}

// SetWebRoot 设置Web根目录
func (f *APIFramework) SetWebRoot(dir string) *APIFramework {
	f.wwwRoot = dir
	return f
}

func (f *APIFramework) BindHandler(prefix string, handler http.Handler) error {
	f.router.Handle(prefix, handler)
	return nil
}
func (f *APIFramework) BindHandlerFunc(prefix string, handler http.HandlerFunc) error {
	f.router.Handle(prefix, handler)
	return nil
}

// RegisterController 注册控制器
func (f *APIFramework) RegisterController(prefix string, controllers ...interface{}) error {
	for _, controller := range controllers {
		controllerType := reflect.TypeOf(controller)
		if controllerType.Kind() != reflect.Ptr {
			return fmt.Errorf("controller must be a pointer to struct, got %T", controller)
		}
		controllerType = controllerType.Elem()
		if controllerType.Kind() != reflect.Struct {
			return fmt.Errorf("controller must be a pointer to struct, got %T", controller)
		}

		controllerValue := reflect.ValueOf(controller).Elem()
		controllerName := controllerType.Name()

		// 存储前缀
		f.prefixes[controllerName] = prefix
		// 存储控制器
		f.controllers[controllerName] = controller

		// 注入 APIFramework 实例
		if field := controllerValue.FieldByName("F"); field.IsValid() && field.Type() == reflect.TypeOf(f) {
			field.Set(reflect.ValueOf(f))
		}

		// 尝试调用 Initialize 方法
		if initializer, ok := controller.(interface{ Initialize(*APIFramework) error }); ok {
			if err := initializer.Initialize(f); err != nil {
				return fmt.Errorf("failed to initialize controller %s: %v", controllerName, err)
			}
		}

		// 自动发现和注册 API
		if err := f.discoverAPIs(controllerName, controller); err != nil {
			return fmt.Errorf("failed to discover APIs for controller %s: %v", controllerName, err)
		}

		if f.debug {
			fmt.Printf("Registered controller: %s with prefix: %s\n", controllerName, prefix)
		}
	}

	return nil
}

// discoverAPIs 自动发现并注册 API
func (f *APIFramework) discoverAPIs(controllerName string, controller interface{}) error {
	controllerType := reflect.TypeOf(controller)
	for i := 0; i < controllerType.NumMethod(); i++ {
		method := controllerType.Method(i)
		if method.Type.NumIn() != 3 || method.Type.NumOut() != 2 {
			continue // 跳过不符合预期签名的方法
		}

		reqType := method.Type.In(2)
		respType := method.Type.Out(0)

		// 检查请求类型是否嵌入了 Meta
		if metaField, ok := reqType.Elem().FieldByName("Meta"); ok {
			metaData := extractMeta(metaField.Tag)
			handlerName := fmt.Sprintf("%s.%s", controllerName, method.Name)

			// 使用前缀构建完整路径
			prefix, _ := f.prefixes[controllerName]
			prefixStr := convert.String(prefix)
			fullPath := strings.TrimRight(prefixStr, "/") + "/" + strings.TrimLeft(metaData["path"], "/")

			apiDef := APIDefinition{
				HandlerName:  handlerName,
				RequestType:  reqType,
				ResponseType: respType,
				Meta: meta.Meta{
					Path:    fullPath,
					Method:  metaData["method"],
					Summary: metaData["summary"],
					Tags:    metaData["tags"],
				},
			}

			f.definitions[handlerName] = apiDef

			if f.debug {
				fmt.Printf("Discovered API: %s %s - %s\n", apiDef.Meta.Method, fullPath, apiDef.Meta.Summary)
			}
		}
	}

	return nil
}

// extractMeta 从字段标签中提取元数据
func extractMeta(tag reflect.StructTag) map[string]string {
	metaData := make(map[string]string)
	for _, key := range []string{"path", "method", "summary", "tags"} {
		if value := tag.Get(key); value != "" {
			metaData[key] = value
		}
	}
	return metaData
}

// injectDependencies 注入依赖（包括框架和 ServiceWeaver 上下文）
func (f *APIFramework) injectDependencies(controller interface{}) {
	controllerValue := reflect.ValueOf(controller).Elem()
	controllerType := controllerValue.Type()

	for i := 0; i < controllerType.NumField(); i++ {
		field := controllerType.Field(i)
		if field.Type == reflect.TypeOf(f) && isExported(field.Name) {
			controllerValue.Field(i).Set(reflect.ValueOf(f))
			if f.debug {
				fmt.Printf("Injected framework instance into %s\n", controllerType.Name())
			}
		} else if service, err := f.GetWeaverService(field.Name); err == nil {
			controllerValue.Set(reflect.ValueOf(service))
		}
	}
}

// isExported 检查字段是否可导出
func isExported(fieldName string) bool {
	return fieldName[0] >= 'A' && fieldName[0] <= 'Z'
}

// GetController 获取已注册的控制器
func (f *APIFramework) GetController(name string) (interface{}, bool) {
	controller, ok := f.controllers[name]
	return controller, ok
}

// createHandler 创建处理函数
func (f *APIFramework) createHandler(def APIDefinition) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// 创建请求对象
		reqValue := reflect.New(def.RequestType.Elem())
		req := reqValue.Interface()
		// 直接初始化 Meta
		if err := meta.InitMeta(req); err != nil {
			http.Error(w, "Failed to initialize request metadata", http.StatusInternalServerError)
			return
		}
		// 根据 HTTP 方法处理请求
		switch r.Method {
		case http.MethodGet:
			err := f.decodeGetRequest(r, req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			if err := f.decodeJSONRequest(r, req); err != nil {
				log.Printf("Error decoding JSON request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		case http.MethodDelete:
			// 对于 DELETE 请求，我们可能需要处理 URL 参数和请求体
			if err := f.decodeDeleteRequest(r, req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, "Unsupported method", http.StatusMethodNotAllowed)
			return
		}

		if f.debug {
			jsonBytes, _ := json.MarshalIndent(req, "", "  ")
			log.Printf("Parsed request object:\n%s", string(jsonBytes))
		}

		if err := g.Validator().Data(req).Run(context.Background()); err != nil {
			log.Printf("Validation error: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := g.Validator().Data(req).Run(context.Background()); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// 获取控制器
		controllerName := strings.Split(def.HandlerName, ".")[0]
		controller := f.controllers[controllerName]

		// 调用控制器方法
		method := reflect.ValueOf(controller).MethodByName(strings.Split(def.HandlerName, ".")[1])
		results := method.Call([]reflect.Value{
			reflect.ValueOf(ctx),
			reqValue,
		})

		// 处理响应
		if len(results) > 1 && !results[1].IsNil() {
			err := results[1].Interface().(error)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results[0].Interface())
	}
}

// decodeJSONRequest 处理 JSON 请求体
func (f *APIFramework) decodeJSONRequest(r *http.Request, dst interface{}) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %v", err)
	}
	defer r.Body.Close()

	if f.debug {
		log.Printf("Raw JSON data:\n%s", string(body))
	}

	// 创建一个临时结构来存储JSON数据
	var tempData map[string]interface{}
	if err := json.Unmarshal(body, &tempData); err != nil {
		return fmt.Errorf("failed to decode JSON: %v", err)
	}

	// 使用反射设置字段
	dstValue := reflect.ValueOf(dst).Elem()
	for i := 0; i < dstValue.NumField(); i++ {
		field := dstValue.Type().Field(i)
		if field.Anonymous {
			continue // 跳过匿名字段（如g.Meta）
		}
		jsonTag := field.Tag.Get("json")
		if jsonTag == "" {
			jsonTag = field.Name
		}
		if value, ok := tempData[jsonTag]; ok {
			if err := setField(dstValue.Field(i), value); err != nil {
				return fmt.Errorf("error setting field %s: %v", field.Name, err)
			}
		}
	}

	if f.debug {
		jsonBytes, _ := json.MarshalIndent(dst, "", "  ")
		log.Printf("Parsed request object:\n%s", string(jsonBytes))
	}

	return nil
}

// decodeDeleteRequest 处理 DELETE 请求
func (f *APIFramework) decodeDeleteRequest(r *http.Request, dst interface{}) error {
	// 首先尝试从 URL 参数解析
	if err := f.decodeGetRequest(r, dst); err != nil {
		return err
	}

	// 如果请求体不为空，也尝试解析 JSON
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
			return err
		}
	}

	return nil
}

func (f *APIFramework) decodeGetRequest(r *http.Request, dst interface{}) error {
	values := r.URL.Query()
	return f.decodeStructFromValues(values, reflect.ValueOf(dst).Elem())
}

func (f *APIFramework) decodeStructFromValues(values url.Values, v reflect.Value) error {
	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		fieldValue := v.Field(i)

		// 处理匿名字段
		if field.Anonymous {
			if field.Type.Kind() == reflect.Struct {
				if err := f.decodeStructFromValues(values, fieldValue); err != nil {
					return err
				}
			} else if field.Type.Kind() == reflect.Ptr && field.Type.Elem().Kind() == reflect.Struct {
				if fieldValue.IsNil() {
					fieldValue.Set(reflect.New(field.Type.Elem()))
				}
				if err := f.decodeStructFromValues(values, fieldValue.Elem()); err != nil {
					return err
				}
			}
			continue
		}

		fieldName, shouldFill := getFieldName(field)
		if !shouldFill {
			continue
		}

		// 处理非匿名的嵌套结构体
		if field.Type.Kind() == reflect.Struct || (field.Type.Kind() == reflect.Ptr && field.Type.Elem().Kind() == reflect.Struct) {
			var structValue reflect.Value
			if field.Type.Kind() == reflect.Ptr {
				if fieldValue.IsNil() {
					fieldValue.Set(reflect.New(field.Type.Elem()))
				}
				structValue = fieldValue.Elem()
			} else {
				structValue = fieldValue
			}

			if err := f.decodeStructFromValues(values, structValue); err != nil {
				return err
			}
			continue
		}

		// 处理切片
		if field.Type.Kind() == reflect.Slice {
			sliceValues := values[fieldName]
			if len(sliceValues) > 0 {
				slice := reflect.MakeSlice(field.Type, len(sliceValues), len(sliceValues))
				for i, sliceValue := range sliceValues {
					if err := setField(slice.Index(i), sliceValue); err != nil {
						return err
					}
				}
				fieldValue.Set(slice)
			}
			continue
		}

		// 处理 map
		if field.Type.Kind() == reflect.Map {
			mapValues := make(map[string][]string)
			prefix := fieldName + "["
			for key, vals := range values {
				if strings.HasPrefix(key, prefix) && strings.HasSuffix(key, "]") {
					mapKey := key[len(prefix) : len(key)-1]
					mapValues[mapKey] = vals
				}
			}
			if len(mapValues) > 0 {
				mapValue := reflect.MakeMap(field.Type)
				for key, vals := range mapValues {
					mapKey := reflect.New(field.Type.Key()).Elem()
					if err := setField(mapKey, key); err != nil {
						return err
					}
					mapVal := reflect.New(field.Type.Elem()).Elem()
					if err := setField(mapVal, vals[0]); err != nil {
						return err
					}
					mapValue.SetMapIndex(mapKey, mapVal)
				}
				fieldValue.Set(mapValue)
			}
			continue
		}

		// 处理基本类型
		value := values.Get(fieldName)
		if value != "" {
			if err := setField(fieldValue, value); err != nil {
				return err
			}
		}
	}

	return nil
}

// getFieldName 获取字段的名称
func getFieldName(field reflect.StructField) (string, bool) {
	if tag, ok := field.Tag.Lookup("p"); ok && tag != "" {
		return tag, true
	}

	if tag, ok := field.Tag.Lookup("json"); ok && tag != "" {
		parts := strings.Split(tag, ",")
		if parts[0] != "" {
			return parts[0], true
		}
	}

	// 如果没有标签，使用字段名
	return field.Name, true
}

// GetServer 返回http.Handler接口，用于启动服务
func (f *APIFramework) GetServer() http.Handler {
	f.initOnce.Do(func() {
		if f.debug {
			log.Println("Initializing framework in GetServer")
		}
		f.Init()
	})
	return f.router
}

func (f *APIFramework) SetPort(addr string) {
	f.addr = addr
}

func (f *APIFramework) Run() {

	log.Fatal(http.ListenAndServe(f.addr, f.GetServer()))
}

// PrintAPIRoutes 输出所有注册的API访问地址
func (f *APIFramework) PrintAPIRoutes() {
	fmt.Println("Registered API Routes:")
	fmt.Println("----------------------")

	var routes []string
	for _, def := range f.definitions {
		route := fmt.Sprintf("%s %s - %s", def.Meta.Method, def.Meta.Path, def.Meta.Summary)
		routes = append(routes, route)
	}

	// 排序路由以便更容易阅读
	sort.Strings(routes)

	for _, route := range routes {
		fmt.Println(route)
	}
	fmt.Println("----------------------")
}
