<?php
// Apache mod_php bootstrap entry point — no superglobals, no
// echo/header at top level. Just defines CONTROLLER_ID, requires
// dependencies, and invokes a service whose method name matches
// the bootstrap-verb set (printScrrCode).
//
// extractPhpApiEndpoints should qualify this as a php.fileBased
// handler via the bootstrap heuristic.

define('CONTROLLER_ID', "SCRR");

require_once("config/mnetc.php");
require_function("service/Scrr/ScrrRenderingService.php");

$scrrRenderingService = new ScrrRenderingService();
$scrrRenderingService->printScrrCode();
