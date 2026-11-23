# 0006. Persistência do progresso e decisões vinculadas à tentativa

## Contexto

Um worker pode gravar progresso a partir de um estado anterior a um pedido de pausa ou cancelamento. O console também pode exibir um gate que já mudou enquanto o operador decidia. A resposta da API de sinais precisa indicar se a entrada já pode ser recuperada.

## Decisão

Gravar progresso pelo `RunsRepo` com revisão e lease verificados na transação, preservando os pedidos gravados de pausa e cancelamento. Persistir os logs e as observações recebidos por `/signals` com os cursores e a revisão do stream na mesma transação e responder HTTP 202 após o commit. Vincular a decisão do console ao status, à tentativa e à contagem de rework do cartão ou nó exibido.

## Por quê

A conferência de revisão e posse impede que atualizações de progresso vindas de um estado desatualizado apaguem comandos do operador. A confirmação após o commit torna recuperáveis os sinais aceitos pela API. A expectativa da decisão impede que uma aprovação ou um pedido de rework em uma tela desatualizada afete outra tentativa.

## Consequências

Em caso de divergência na expectativa da decisão, a API responde HTTP 409 com `run_decision_stale` e o console informa a divergência. A confirmação durável se aplica a `/signals`. O recebedor OTLP mantém seu contrato de melhor esforço.
