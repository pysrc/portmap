# Golang内网端口映射实现

bilibili地址：https://www.bilibili.com/video/BV1TD4y1d7s4/

![原理示例](原理示例.png)

# 配置说明

**json配置文件中含"-"的为非必配字段**


```json
{
    "server": {
        "key": "helloworld", // 客户端与服务端必须对应，且用于数据加密
        "port": 8808, // 服务端控制端口
        "-limit-port": [ // 留给客户端选择的端口范围
            9100,
            9110
        ]
    },
    "client": {
        "key": "helloworld", // 客户端与服务端必须对应，且用于数据加密
        "server": "127.0.0.1:8808", // 服务端IP与端口
        "map": [ // 内网映射到服务端的规则
            {
                "inner": "127.0.0.1:6379", // 内网地址
                "outer": 9100 // 映射到服务端的端口
            },
            {
                "inner": "127.0.0.1:6379",
                "outer": 9101
            }
        ]
    }
}

```
