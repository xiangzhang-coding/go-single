# 秒杀削峰使用 RabbitMQ，Kafka 进 backlog

RabbitMQ 的 AMQP 概念（exchange/queue/binding）对新手友好，Docker 与控制台体验好，足以承载秒杀削峰；Kafka 的分区/消费者组模型偏分布式大厂场景，学习成本高，列入 backlog。借助 ADR-0003 的消息层接口，二期可低成本替换验证。
