package nf

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/go-openapi/spec"
	"github.com/sagoo-cloud/nexframe/nf/g"
	"gorm.io/gorm"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// GenerateSwaggerJSON 生成完整的 Swagger JSON
func (f *APIFramework) GenerateSwaggerJSON() (string, error) {
	log.Println("Generating Swagger JSON")

	// 重新初始化 Swagger 规范
	f.initSwaggerSpec()

	// 重新生成所有 API 定义
	for _, def := range f.definitions {
		f.updateSwaggerSpec(def)
	}

	// 将 Swagger 规范转换为 JSON
	swaggerJSON, err := json.MarshalIndent(f.swaggerSpec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("error marshaling Swagger JSON: %v", err)
	}
	if f.debug {
		log.Printf("Generated Swagger JSON: %s", string(swaggerJSON))

	}
	return string(swaggerJSON), nil
}

func (f *APIFramework) initSwaggerSpec() {
	f.swaggerSpec = &spec.Swagger{
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
	}
}

// generateParameters 生成 Swagger 参数定义
func (f *APIFramework) generateParameters(reqType reflect.Type) []spec.Parameter {
	var params []spec.Parameter
	processedTypes := make(map[reflect.Type]bool)

	var generateParams func(t reflect.Type, prefix string)
	generateParams = func(t reflect.Type, prefix string) {
		if processedTypes[t] {
			return // 避免循环引用
		}
		processedTypes[t] = true

		t = deref(t)
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)

			// 跳过 g.Meta 字段
			if field.Anonymous && field.Type == reflect.TypeOf(g.Meta{}) {
				continue
			}

			jsonTag := field.Tag.Get("json")
			if jsonTag == "" {
				jsonTag = strings.ToLower(field.Name)
			}
			jsonTag = strings.Split(jsonTag, ",")[0] // 处理 json tag 中的选项

			paramName := prefix + jsonTag

			if field.Anonymous || (field.Type.Kind() == reflect.Struct && field.Type != reflect.TypeOf(time.Time{})) {
				// 处理嵌入字段和嵌套结构
				generateParams(field.Type, prefix)
			} else {
				param := spec.Parameter{
					ParamProps: spec.ParamProps{
						Name:        paramName,
						In:          "query",
						Description: field.Tag.Get("description"),
						Required:    strings.Contains(field.Tag.Get("v"), "required"),
					},
					SimpleSchema: spec.SimpleSchema{
						Type:   f.getSwaggerType(field.Type),
						Format: f.getSwaggerFormat(field.Type),
					},
				}

				// 处理指针类型
				if field.Type.Kind() == reflect.Ptr {
					param.SimpleSchema.Type = f.getSwaggerType(field.Type.Elem())
					param.VendorExtensible.AddExtension("x-nullable", true)
				}

				// 处理数组类型
				if field.Type.Kind() == reflect.Slice || field.Type.Kind() == reflect.Array {
					param.Type = "array"
					param.Items = &spec.Items{
						SimpleSchema: spec.SimpleSchema{
							Type: f.getSwaggerType(field.Type.Elem()),
						},
					}
				}

				params = append(params, param)
			}
		}
	}

	generateParams(reqType, "")
	return params
}

func (f *APIFramework) getSwaggerType(t reflect.Type) string {
	t = deref(t)
	switch t.Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.String:
		return "string"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Struct:
		if t == reflect.TypeOf(time.Time{}) {
			return "string"
		}
		return "object"
	default:
		return "string"
	}
}

func (f *APIFramework) getSwaggerFormat(t reflect.Type) string {
	t = deref(t)
	switch t.Kind() {
	case reflect.Int64, reflect.Uint64:
		return "int64"
	case reflect.Int32, reflect.Uint32:
		return "int32"
	case reflect.Float32:
		return "float"
	case reflect.Float64:
		return "double"
	default:
		if t == reflect.TypeOf(time.Time{}) {
			return "date-time"
		}
		return ""
	}
}

func deref(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Ptr {
		return t.Elem()
	}
	return t
}

// generateResponses 生成 Swagger 响应定义
func (f *APIFramework) generateResponses(respType reflect.Type) *spec.Responses {
	return &spec.Responses{
		ResponsesProps: spec.ResponsesProps{
			StatusCodeResponses: map[int]spec.Response{
				200: {
					ResponseProps: spec.ResponseProps{
						Description: "Successful response",
						Schema: &spec.Schema{
							SchemaProps: spec.SchemaProps{
								Ref: spec.MustCreateRef("#/definitions/" + respType.Elem().Name()),
							},
						},
					},
				},
			},
		},
	}
}

func (f *APIFramework) generateModelDefinition(swagger *spec.Swagger, modelType reflect.Type, name string) {
	modelType = deref(modelType) // 处理指针类型
	properties := make(map[string]spec.Schema)

	for i := 0; i < modelType.NumField(); i++ {
		field := modelType.Field(i)
		if field.Anonymous {
			// 处理匿名字段，如 g.Meta
			continue
		}

		jsonTag := field.Tag.Get("json")
		if jsonTag == "" {
			jsonTag = strings.ToLower(field.Name)
		}
		jsonTag = strings.Split(jsonTag, ",")[0] // 处理 json tag 中的选项

		fieldType := field.Type
		fieldSchema := f.getFieldSchema(swagger, fieldType, name+"_"+field.Name)

		fieldSchema.SchemaProps.Description = field.Tag.Get("description")
		properties[jsonTag] = fieldSchema
	}

	swagger.Definitions[name] = spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type:       []string{"object"},
			Properties: properties,
		},
	}
}

func (f *APIFramework) getFieldSchema(swagger *spec.Swagger, fieldType reflect.Type, name string) spec.Schema {
	fieldType = deref(fieldType) // 处理指针类型

	switch fieldType.Kind() {
	case reflect.Struct:
		if fieldType == reflect.TypeOf(time.Time{}) {
			return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "date-time"}}
		}
		if fieldType == reflect.TypeOf(gorm.DeletedAt{}) {
			return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "date-time", Nullable: true}}
		}
		f.generateModelDefinition(swagger, fieldType, name)
		return spec.Schema{SchemaProps: spec.SchemaProps{Ref: spec.MustCreateRef("#/definitions/" + name)}}
	case reflect.Slice:
		itemSchema := f.getFieldSchema(swagger, fieldType.Elem(), name+"Item")
		return spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type:  []string{"array"},
				Items: &spec.SchemaOrArray{Schema: &itemSchema},
			},
		}
	default:
		return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{f.getSwaggerType(fieldType)}}}
	}
}

func (f *APIFramework) saveSwaggerJSON() {
	// 生成 Swagger JSON
	swaggerJson, err := f.GenerateSwaggerJSON()
	if err != nil {
		log.Fatalf("Error generating Swagger JSON: %v", err)
	}
	log.Println("swaggerJson:", swaggerJson)
	// 获取当前工作目录
	currentDir, err := os.Getwd()
	if err != nil {
		fmt.Println("Error getting current directory:", err)
		return
	}

	// 文件名
	fileName := "doc.json"

	// 完整的文件路径
	fullFilePath := filepath.Join(currentDir, fileName)

	// 打开文件，如果文件不存在则创建它
	file, err := os.Create(fullFilePath)
	if err != nil {
		fmt.Println("Error creating file:", err)
		return
	}
	defer file.Close() // 确保在函数结束时关闭文件
	// 使用bufio.NewWriter来创建一个写入缓冲
	writer := bufio.NewWriter(file)
	defer writer.Flush() // 确保在函数结束时刷新缓冲

	// 写入字符串到文件
	_, err = writer.WriteString(swaggerJson)
	if err != nil {
		fmt.Println("Error writing to file:", err)
		return
	}
	// 显式刷新缓冲区，确保数据写入文件
	err = writer.Flush()
	if err != nil {
		fmt.Println("Error flushing buffer to file:", err)
		return
	}

	fmt.Printf("String successfully written to %s\n", fullFilePath)
}

// addSwaggerPath 添加路径到 Swagger 规范
func (f *APIFramework) addSwaggerPath(def APIDefinition) {
	path := f.swaggerSpec.Paths.Paths[def.Meta.Path]
	operation := &spec.Operation{
		OperationProps: spec.OperationProps{
			Summary:     def.Meta.Summary,
			Description: def.Meta.Summary,
			Tags:        strings.Split(def.Meta.Tags, ","),
			Parameters:  f.getSwaggerParams(def.RequestType),
			Responses:   f.getSwaggerResponses(def.ResponseType),
		},
	}

	switch strings.ToUpper(def.Meta.Method) {
	case "GET":
		path.Get = operation
	case "POST":
		path.Post = operation
	case "PUT":
		path.Put = operation
	case "DELETE":
		path.Delete = operation
	}

	f.swaggerSpec.Paths.Paths[def.Meta.Path] = path
}

// getSwaggerParams 从请求类型生成 Swagger 参数
func (f *APIFramework) getSwaggerParams(reqType reflect.Type) []spec.Parameter {
	var params []spec.Parameter
	for i := 0; i < reqType.Elem().NumField(); i++ {
		field := reqType.Elem().Field(i)
		if field.Anonymous {
			continue
		}
		param := spec.Parameter{
			ParamProps: spec.ParamProps{
				Name:        field.Tag.Get("json"),
				In:          "query",
				Description: field.Tag.Get("description"),
				Required:    strings.Contains(field.Tag.Get("v"), "required"),
			},
			SimpleSchema: spec.SimpleSchema{
				Type: field.Type.String(),
			},
		}
		params = append(params, param)
	}
	return params
}

// getSwaggerResponses 从响应类型生成 Swagger 响应
func (f *APIFramework) getSwaggerResponses(respType reflect.Type) *spec.Responses {
	return &spec.Responses{
		ResponsesProps: spec.ResponsesProps{
			StatusCodeResponses: map[int]spec.Response{
				200: {
					ResponseProps: spec.ResponseProps{
						Description: "Successful response",
						Schema: &spec.Schema{
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
							},
						},
					},
				},
			},
		},
	}
}

// serveSwaggerSpec 提供 Swagger 规范 JSON
func (f *APIFramework) serveSwaggerSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(f.swaggerSpec)
}
