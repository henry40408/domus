Feature: Logging in and out

  Background:
    Given a domus server with the admin "demo" and the Hue bridge paired

  Scenario: Logging in
    When I open the app
    And I log in as "demo" with password "demo-password-123"
    Then I should see the dashboard for "demo"

  Scenario: A wrong password is refused
    When I open the app
    And I log in as "demo" with password "not-the-password"
    Then I should see the toast "Wrong username or password."
    And I should see the login form

  Scenario: Logging out
    Given I am logged in as "demo"
    When I log out
    Then I should see the login form
