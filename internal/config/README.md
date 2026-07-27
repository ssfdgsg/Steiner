# internal/config/ — 配置加载与校验

## 职责
- 读取 YAML（`gopkg.in/yaml.v3`）反序列化为强类型结构体（`Config` 及各配置段）；
- **默认值**：`ApplyDefaults` 填充全部缺省（含自动注入内置 `default` 策略），
  下游模块无需判空；
- **静态校验**：`Validate` 在启动期暴露一切引用错误——后端 id 唯一、engine 枚举、
  模型路由引用的后端/子池/PD 组/策略存在、PD 链路不引用组外后端、
  backends/splits/pd_group 互斥、告警与扩缩容引用的 webhook/路由存在等；
- 时长类型 `Duration`：支持 Go duration 字符串（`500ms`）与纯数字（按秒）。

## 热更新语义
| 项 | 方式 |
|---|---|
| 策略表达式 | `PUT /admin/policies/{name}` 运行期热更（policy 引擎原子替换） |
| 后端隔离 | `POST /admin/backends/{id}/cordon|uncordon` |
| 其余配置段 | 修改文件后重启（或 K8s 滚动重启）生效 |

## 接口
```go
func Load(path string) (*Config, error)   // 读取 + 默认值 + 校验
func (c *Config) ApplyDefaults()
func (c *Config) Validate() error
```

## 文件
`config.go`（结构体 + 默认值 + 校验）、`config_test.go`（加载/默认值/各类非法配置用例）。
