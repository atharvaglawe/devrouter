<?php
// Bootstrap-style PHP entry point — Apache mod_php serves it at
// /scrr.php directly. The PHP AST fallback should qualify this
// via the bootstrap heuristic (define(CONTROLLER_ID) + service
// invocation) and emit a basename-preserving Route at /scrr.php.

define('CONTROLLER_ID', "SCRR");

require_once("config/mnetc.php");
require_once("service/ScrrRenderingService.php");

$scrrRenderingService = new ScrrRenderingService();
$scrrRenderingService->printScrrCode();
