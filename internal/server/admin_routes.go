package server

// registerManagement adds the routes that manage lights, groups, tokens, app
// devices, users and the Hue bridge. Sensitive actions (new tokens, users,
// pairing, group changes) also need a recent password when the session came
// from an app's token.
func (a *App) registerManagement(routes *adminRoutes) {
	routes.route("GET /lights", needSession, a.listLights)
	routes.route("GET /rooms", needSession, a.listRooms)
	routes.route("POST /devices/test", needSession, a.testDevice)
	routes.route("GET /groups", needSession, a.listGroups)
	routes.route("GET /tokens", needSession, a.listTokens)
	routes.route("DELETE /tokens/{id}", needSession, a.revokeToken)
	routes.route("GET /apps", needSession, a.listApps)
	routes.route("DELETE /apps/{id}", needSession, a.revokeApp)

	routes.route("POST /tokens", needSession|needRecentAuth, a.createToken)

	routes.route("GET /hue", needAdmin, a.hueStatus)
	routes.route("GET /users", needAdmin, a.listUsers)

	const adminSensitive = needAdmin | needRecentAuth
	routes.route("POST /hue/pair", adminSensitive, a.pairHue)
	routes.route("POST /users", adminSensitive, a.createUser)
	routes.route("PUT /users/{id}", adminSensitive, a.updateUser)
	routes.route("DELETE /users/{id}", adminSensitive, a.deleteUser)
	// Group membership decides what scoped users and tokens may control.
	routes.route("PUT /groups/{name}", adminSensitive, a.putGroup)
	routes.route("DELETE /groups/{name}", adminSensitive, a.deleteGroup)
}
