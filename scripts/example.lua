-- ============================================================
-- PL Proxy — Script de Exemplo
-- Demonstra todas as funcionalidades da API Lua
--
-- Para rodar: .run example.lua
-- Para debug: .debug example.lua
-- Para ver IDs: .ids
-- Para parar:  .stop <id>
-- ============================================================

-- Obtém o ID deste script
local my_id = proxy.get_script_id()
proxy.log("Script de exemplo carregado! ID: #" .. my_id)

-- ════════════════════════════════════════════════════════════
--  Comandos Personalizados
-- ════════════════════════════════════════════════════════════

-- Comando .fullbright — Visão noturna
local fullbright_on = false

proxy.register_command("fullbright", "Toggle visão noturna", function(args)
    fullbright_on = not fullbright_on
    local eid = proxy.get_entity_runtime_id()

    if fullbright_on then
        proxy.send_packet_to_client("MobEffect", {
            entity_runtime_id = eid,
            operation = proxy.EFFECT_ADD,
            effect_type = 16,       -- Night Vision
            amplifier = 0,
            particles = false,
            duration = 2147483647   -- MaxInt32
        })
        proxy.send_message("§a[Fullbright] §fVisão noturna §aATIVADA§f.")
    else
        proxy.send_packet_to_client("MobEffect", {
            entity_runtime_id = eid,
            operation = proxy.EFFECT_REMOVE,
            effect_type = 16
        })
        proxy.send_message("§c[Fullbright] §fVisão noturna §cDESATIVADA§f.")
    end
end, {"fb"})

-- Comando .freecam — Câmera livre
local freecam_on = false

proxy.register_command("freecam", "Toggle câmera livre", function(args)
    freecam_on = not freecam_on

    if freecam_on then
        proxy.send_packet_to_client("SetTitle", {
            action = proxy.TITLE_SET_SUBTITLE,
            text = "§aCâmera livre ATIVADA",
            stay = 40
        })
        proxy.send_message("§a[FreeCam] §fCâmera livre §aATIVADA§f.")
    else
        proxy.send_packet_to_client("SetTitle", {
            action = proxy.TITLE_SET_SUBTITLE,
            text = "§cCâmera livre DESATIVADA",
            stay = 40
        })
        proxy.send_message("§c[FreeCam] §fCâmera livre §cDESATIVADA§f.")
    end
end, {"fc"})

-- Comando .nochat — Silenciar chat
local nochat_on = false

proxy.register_command("nochat", "Toggle silenciar chat", function(args)
    nochat_on = not nochat_on
    if nochat_on then
        proxy.send_packet_to_client("SetTitle", { action = proxy.TITLE_CLEAR })
        proxy.send_packet_to_client("SetTitle", { action = proxy.TITLE_RESET })
        proxy.send_message("§a[NoChat] §fChat §aBLOQUEADO§f.")
    else
        proxy.send_message("§c[NoChat] §fChat §cLIBERADO§f.")
    end
end, {"nc"})

-- Comando .coords — Mostrar coordenadas
proxy.register_command("coords", "Mostra suas coordenadas", function(args)
    local pos = proxy.get_position()
    proxy.send_message(string.format(
        "§b[Coords] §fX: §e%.1f §fY: §e%.1f §fZ: §e%.1f",
        pos.x, pos.y, pos.z
    ))
end, {"co"})

-- Comando .clip — Teleportar N blocos acima
proxy.register_command("clip [n]", "Teleporta N blocos acima (padrão 3)", function(args)
    local blocks = 3
    if #args >= 1 then
        blocks = tonumber(args[1]) or 3
    end

    if blocks > 256 then
        proxy.send_message("§c[Clip] §fMáximo de 256 blocos por vez.")
        return
    end

    local pos = proxy.get_position()
    local new_y = pos.y + blocks
    proxy.set_position(pos.x, new_y, pos.z)
    proxy.send_message(string.format(
        "§a[Clip] §fTeleportado §e%.0f§f blocos acima. Novo Y: §e%.1f",
        blocks, new_y
    ))
end, {"cl"})

-- Comando .server — Mostrar servidor
proxy.register_command("server", "Mostra o servidor atual", function(args)
    local name = proxy.get_server_name()
    local addr = proxy.get_server_address()
    proxy.send_message(string.format("§b[Server] §f%s §7— §f%s", name, addr))
end, {"srv"})

-- Comando .hora — Hora do sistema
proxy.register_command("hora", "Hora atual do sistema", function(args)
    local now = os.date("*t")
    proxy.send_message(string.format(
        "§b[Hora] §f%02d:%02d:%02d §7(%02d/%02d/%04d)",
        now.hour, now.min, now.sec,
        now.day, now.month, now.year
    ))
end, {"time"})

-- Comando .echo — Ecoa a mensagem de volta
proxy.register_command("echo", "Ecoa uma mensagem", function(args)
    if #args == 0 then
        proxy.send_message("§c[Uso] §f.echo §e<mensagem>")
        return
    end
    proxy.send_message("§f" .. table.concat(args, " "))
end)

-- Comando .info — Mostra informações deste script
proxy.register_command("info", "Mostra informações deste script", function(args)
    proxy.send_message(string.format(
        "§b[Info] §fScript #%d: §e%s",
        my_id, "example.lua"
    ))
    proxy.send_message("§b[Info] §fUse §e.ids§f para ver todos os scripts")
    proxy.send_message("§b[Info] §fUse §e.stop %d§f para parar este script", my_id)
end)

-- ════════════════════════════════════════════════════════════
--  Hooks de Pacotes
-- ════════════════════════════════════════════════════════════

-- Hook: intercepta mensagens do chat no servidor
proxy.on_server_packet(function(name, data)
    -- Bloqueia chat quando nochat está ativo
    if name == "Text" and data.type ~= 0 then
        if nochat_on then
            return true
        end
    end

    -- Bloqueia títulos quando nochat está ativo
    if name == "SetTitle" and nochat_on then
        return true
    end

    -- Bloqueia boss events quando nochat está ativo
    if name == "BossEvent" and nochat_on then
        return true
    end

    return false
end)

-- Hook: loga pacotes do cliente (para debug)
-- Descomente para ver todos os pacotes:
-- proxy.on_client_packet(function(name, data)
--     proxy.log("Client → " .. name)
--     return false
-- end)

proxy.log("Comandos registrados: .fullbright, .freecam, .nochat, .coords, .clip, .server, .hora, .echo, .info")
