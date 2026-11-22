(() => {
 const content=document.getElementById('content');
 const escape=s=>String(s||'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
 let token=new URLSearchParams(location.hash.slice(1)).get('request')||sessionStorage.getItem('wecomBindingToken')||'';
 if(token){sessionStorage.setItem('wecomBindingToken',token);history.replaceState(null,'',location.pathname);}
 async function request(path,body){const response=await fetch(path,{method:body?'POST':'GET',headers:body?{'Content-Type':'application/json'}:{},body:body?JSON.stringify(body):undefined,credentials:'same-origin',cache:'no-store'});const data=await response.json();if(!response.ok)throw new Error(data.error||'O pedido não foi concluído');return data;}
 function failure(error){content.innerHTML=`<p class="error">${escape(error.message)}</p><p>Volte ao chat com o bot e envie /bind para receber um link novo.</p>`;}
 async function start(){
  if(!/^[a-f0-9]{64}$/.test(token))throw new Error('Link de vínculo inválido');
  const auth=await request('/api/auth/current?return_url='+encodeURIComponent(location.origin+location.pathname));
  if(!auth.user||auth.user.isGuest){
   content.innerHTML='<p>Entre com a sua própria conta da plataforma. Depois do login, quem mandou a mensagem no chat ainda precisa confirmar para o vínculo valer.</p><button id="login">Entrar na plataforma</button><button class="secondary" id="local" hidden>Login local de teste</button>';
   document.getElementById('login').onclick=()=>{if(auth.loginUrl)location.href=auth.loginUrl;else failure(new Error('Login indisponível'));};
   const local=await request('/api/auth/local').catch(()=>({enabled:false}));
   if(local.enabled){const button=document.getElementById('local');button.hidden=false;button.onclick=()=>request('/api/auth/local',{}).then(start).catch(failure);}
   return;
  }
  const data=await request('/api/bot-bindings/status',{token});
  if(data.status==='confirmed'){complete(data.loginName);return;}
  content.innerHTML=`<p>Conta atual da plataforma</p><p id="account">${escape(auth.user.chineseName)} · ${escape(data.loginName)}</p><p>Depois do vínculo, o bot usa as credenciais pessoais e as permissões de projeto desta conta para executar seus pedidos. Confirme que esta conta é sua.</p><button id="claim">Confirmar conta</button><p><small>Se outra pessoa do grupo clicar no link, o vínculo não é concluído.</small></p>`;
  document.getElementById('claim').onclick=async()=>{const button=document.getElementById('claim');button.disabled=true;try{const result=await request('/api/bot-bindings/claim',{token});if(result.status==='confirmed'){complete(result.loginName);return;}content.innerHTML=`<p>Último passo: confirmar no chat</p><p>Copie o comando abaixo e mande ao bot que enviou o link, no seu próprio chat com ele (no grupo do WeCom, mencione o bot). Nunca mande o código para outra pessoa.</p><code id="command">${escape(result.command)}</code><p><button id="copy">Copiar comando</button></p><p>A conta vinculada será <strong>${escape(result.loginName)}</strong>. Não repasse o código.</p><small>Aguardando a confirmação no chat. A página atualiza sozinha…</small>`;document.getElementById('copy').onclick=()=>navigator.clipboard.writeText(result.command).then(()=>{document.getElementById('copy').textContent='Copiado';}).catch(()=>{document.getElementById('copy').textContent='Selecione o comando acima para copiar';});poll(result.loginName);}catch(error){failure(error);}};
 }
 function complete(login){sessionStorage.removeItem('wecomBindingToken');content.innerHTML=`<h2>Vínculo concluído</h2><p>A conta do chat agora está vinculada a <strong>${escape(login)}</strong>.</p><p>Volte ao bot e envie de novo o pedido anterior. Você pode desvincular nas configurações pessoais da plataforma.</p><a class="button" href="/#/settings">Ver configurações pessoais</a>`;}
 function poll(login){const timer=setInterval(async()=>{try{const data=await request('/api/bot-bindings/status',{token});if(data.status==='confirmed'){clearInterval(timer);complete(login);}}catch(error){clearInterval(timer);failure(error);}},2500);}
 start().catch(failure);
})();
