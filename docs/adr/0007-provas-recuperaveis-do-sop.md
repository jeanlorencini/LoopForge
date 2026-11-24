# 0007. Provas recuperáveis do SOP e reconciliação da entrega

## Contexto

O SOP pode precisar retomar uma run após a reconstrução do workspace ou a perda da resposta do provedor de merge. Para avançar, ele precisa recuperar a reserva, comprovar os critérios executados e identificar o PR autorizado. Em um bind do Docker no Windows, renomear o diretório de preparação da reserva pode falhar com `EACCES`.

## Decisão

Guardar a reserva e as provas mínimas em `.sop/workflow-state`, no bind gravável, fora da árvore entregue e dentro do checkpoint da run. Publicar a reserva pela troca atômica de um arquivo que aponta para a preparação completa e selada. Na leitura, conferir se o destino pertence à área da run e se o selo é válido. Registrar, em cada invocação da suíte congelada, os resultados dos critérios declarados em `# SOP-CASE` em um recibo fresco vinculado à execução. Preservar o vínculo das provas com o selo, as tentativas e as árvores aprovadas. Antes da chamada de merge, registrar o número do PR e os SHAs verificados na autorização. No retry, consultar o mesmo PR e verificar a ancestralidade Git antes de confirmar a entrega.

## Por quê

O arquivo permite publicar a reserva completa mesmo quando o bind impede renomear a pasta. O checkpoint conserva as provas exigidas na retomada. O recibo por invocação comprova quais critérios realmente rodaram. O PR e os SHAs gravados antes da chamada permitem reconhecer o merge já efetuado quando a resposta se perde.

## Consequências

Uma execução com zero resultados, um critério obrigatório ausente ou um caso ignorado não pode ser aprovada. O checkpoint exclui os dados efêmeros de autenticação do executor. A execução permanece no ambiente vinculado à run. A recuperação de uma reserva removida exige o conteúdo aprovado de origem. A implantação combina uma aplicação que suporte as opções do workflow com a definição derivada. O replay foi comprovado com exports compatíveis do SOP Codex em execução direta.
