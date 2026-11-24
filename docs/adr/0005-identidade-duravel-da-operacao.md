# 0005. Identidade durável da operação e posse da run

## Contexto

Uma interrupção pode ocorrer depois que o motor despacha uma tarefa, enquanto o runner a executa ou depois que ele grava o resultado. O worker que assume a run precisa identificar a tarefa dessa operação para continuar a mesma tentativa. Um nó pode despachar mais de uma operação na mesma tentativa.

## Decisão

Correlacionar a tarefa durável pela run, pelo nó, pela identidade da tentativa e pela posição da operação. Na retomada, adotar a tarefa correspondente com seu ID, conteúdo, lease e estado. Reaproveitar o resultado durável quando a tarefa já tiver terminado. Verificar a posse da run e a tentativa atual sob o bloqueio da run ao inserir uma tarefa. Na retomada `all`, aplicar a mudança de estado com o lease adquirido e a revisão conferida. A API sem executor mantém a conferência pela revisão.

## Por quê

A correlação permite que o novo worker aguarde a mesma tarefa ou use seu resultado depois de uma interrupção. A verificação dentro da gravação impede que um worker que perdeu a posse despache para uma tentativa antiga.

## Consequências

Registros antigos podem ser adotados quando a correspondência for verificável. Uma correspondência ambígua recebe diagnóstico e fica disponível para o retry explícito do operador, que abre uma nova tentativa. O runner mantém o protocolo de conclusão instalado. A proteção da confirmação pelo número da tentativa depende do runner atualizado.
